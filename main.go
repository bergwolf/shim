// Copyright 2017 HyperHQ Inc.
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log/syslog"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"time"

	opentracing "github.com/opentracing/opentracing-go"
	"github.com/sirupsen/logrus"
	lSyslog "github.com/sirupsen/logrus/hooks/syslog"
	context "golang.org/x/net/context"
)

const (
	shimName    = "kata-shim"
	exitFailure = 1
	exitSuccess = 0
	// Max number of threads the shim should consume.
	// We choose 6 as we want a couple of threads for the runtime (gc etc.)
	// and couple of threads for our parallel user code, such as the copy
	// code in shim.go
	maxThreads = 6
)

// version is the shim version. This variable is populated at build time.
var version = "unknown"

var debug bool

// if true, coredump when an internal error occurs or a fatal signal is received
var crashOnError = false

// if true, enable opentracing support.
var tracing = false

var shimLog *logrus.Entry

func logger() *logrus.Entry {
	if shimLog != nil {
		return shimLog
	}
	return logrus.NewEntry(logrus.StandardLogger())
}

func initLogger(logLevel, container, execID string, announceFields logrus.Fields, loggerOutput io.Writer) error {
	shimLog = logrus.WithFields(logrus.Fields{
		"name":      shimName,
		"pid":       os.Getpid(),
		"source":    "shim",
		"container": container,
		"exec-id":   execID,
	})

	shimLog.Logger.Formatter = &logrus.TextFormatter{TimestampFormat: time.RFC3339Nano}

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		return err
	}

	shimLog.Logger.SetLevel(level)

	shimLog.Logger.Out = loggerOutput

	hook, err := lSyslog.NewSyslogHook("", "", syslog.LOG_INFO|syslog.LOG_USER, shimName)
	if err == nil {
		shimLog.Logger.AddHook(hook)
	}

	logger().WithFields(announceFields).Info("announce")

	return nil
}

func setThreads() {
	// If GOMAXPROCS has not been set, restrict our thread usage
	// so we don't grow many idle threads on large core count systems,
	// which un-necessarily consume host PID space (and thus set an
	// artificial max limit on the number of concurrent containers we can
	// run)
	if os.Getenv("GOMAXPROCS") == "" {
		if runtime.NumCPU() > maxThreads {
			runtime.GOMAXPROCS(maxThreads)
		}
	}
}

func realMain(ctx context.Context) (exitCode int) {
	var (
		logLevel      string
		agentAddr     string
		container     string
		execID        string
		terminal      bool
		proxyExitCode bool
		showVersion   bool
	)

	setThreads()

	flag.BoolVar(&debug, "debug", false, "enable debug mode")
	flag.BoolVar(&tracing, "trace", false, "enable opentracing support")
	flag.BoolVar(&showVersion, "version", false, "display program version and exit")
	flag.StringVar(&logLevel, "log", "warn", "set shim log level: debug, info, warn, error, fatal or panic")
	flag.StringVar(&agentAddr, "agent", "", "agent gRPC socket endpoint")

	flag.StringVar(&container, "container", "", "container id for the shim")
	flag.StringVar(&execID, "exec-id", "", "process id for the shim")
	flag.BoolVar(&terminal, "terminal", false, "specify if a terminal is setup")
	flag.BoolVar(&proxyExitCode, "proxy-exit-code", true, "proxy exit code of the process")

	flag.Parse()

	if showVersion {
		fmt.Printf("%v version %v\n", shimName, version)
		return exitSuccess
	}

	if logLevel == "debug" {
		debug = true
	}

	if debug {
		crashOnError = true
	}

	if agentAddr == "" || container == "" || execID == "" {
		logger().WithField("agentAddr", agentAddr).WithField("container", container).WithField("exec-id", execID).Error("container ID, exec ID and agent socket endpoint must be set")
		return exitFailure
	}

	announceFields := logrus.Fields{
		"version":         version,
		"debug":           debug,
		"log-level":       logLevel,
		"agent-socket":    agentAddr,
		"terminal":        terminal,
		"proxy-exit-code": proxyExitCode,
		"tracing":         tracing,
	}

	// The final parameter makes sure all output going to stdout/stderr is discarded.
	err := initLogger(logLevel, container, execID, announceFields, ioutil.Discard)
	if err != nil {
		logger().WithError(err).WithField("loglevel", logLevel).Error("invalid log level")
		return exitFailure
	}

	// Initialise tracing now the logger is ready
	tracer, err := createTracer(shimName)
	if err != nil {
		logger().WithError(err).Fatal("failed to setup tracing")
		return exitFailure
	}

	// create root span
	span := tracer.StartSpan("realMain")
	ctx = opentracing.ContextWithSpan(ctx, span)
	defer span.Finish()

	shim, err := newShim(ctx, agentAddr, container, execID)
	if err != nil {
		logger().WithError(err).Error("failed to create new shim")
		return exitFailure
	}

	// winsize
	if terminal {
		termios, err := setupTerminal(int(os.Stdin.Fd()))
		if err != nil {
			logger().WithError(err).Error("failed to set raw terminal")
			return exitFailure
		}
		defer restoreTerminal(int(os.Stdin.Fd()), termios)
	}

	// signals
	sigc := shim.handleSignals(ctx, os.Stdin)
	defer signal.Stop(sigc)

	// This wait call cannot be deferred and has to wait for every
	// input/output to return before the code tries to go further
	// and wait for the process. Indeed, after the process has been
	// waited for, we cannot expect to do any more calls related to
	// this process since it is going to be removed from the agent.
	wg := &sync.WaitGroup{}

	// Encapsulate the call the I/O handling function in a span here since
	// that function returns quickly and we want to know how long I/O
	// took.
	stdioSpan, _ := trace(ctx, "proxyStdio")

	// Add a tag to allow the I/O to be filtered out.
	stdioSpan.SetTag("category", "interactive")

	shim.proxyStdio(wg, terminal)

	wg.Wait()

	stdioSpan.Finish()

	// wait until exit
	exitcode, err := shim.wait()
	if err != nil {
		logger().WithError(err).WithField("exec-id", execID).Error("failed waiting for process")
		return exitFailure
	} else if proxyExitCode {
		logger().WithField("exitcode", exitcode).Info("using shim to proxy exit code")
		if exitcode != 0 {
			return int(exitcode)
		}
	}

	return exitSuccess
}

func main() {
	// create a new empty context
	ctx := context.Background()

	defer handlePanic(ctx)

	exitCode := realMain(ctx)

	stopTracing(ctx)

	os.Exit(exitCode)
}
