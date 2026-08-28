package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"
	// Third-party packages
	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"github.com/lmittmann/tint"
	"github.com/mattn/go-isatty"
	pkgerr "github.com/pkg/errors"
	"gopkg.in/natefinch/lumberjack.v2"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

type closeFunc func() error

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

var sensitiveKeys = []string{"password", "key", "apikey", "secret", "pin", "creditcardno", "user"}

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	// setup default console stderr handler (debug level)
	handlers := []slog.Handler{
		tint.NewTextHandler(os.Stderr, &tint.Options{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
			NoColor:     !(isatty.IsCygwinTerminal(os.Stderr.Fd()) || isatty.IsTerminal(os.Stderr.Fd())),
		}),
	}
	closers := []closeFunc{}

	if logFile != "" {
		logger := &lumberjack.Logger{
			Filename:   logFile,
			MaxSize:    1,
			MaxAge:     28,
			MaxBackups: 10,
			LocalTime:  false,
			Compress:   true,
		}
		handlers = append(handlers, slog.NewJSONHandler(logger, &slog.HandlerOptions{
			ReplaceAttr: replaceAttr,
		}))
		closers = append(closers, logger.Close)
	}

	closer := func() error {
		var errs []error
		for _, close := range closers {
			if err := close(); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	}

	return slog.New(slog.NewMultiHandler(handlers...)), closer, nil
}

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("message", err.Error()),
	}
	attrs = append(attrs, linkoerr.Attrs(err)...)

	if tracer, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.String("stack_trace", fmt.Sprintf("%+v", tracer.StackTrace())))
	}
	return attrs
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	// redact sensitive keys
	if slices.Contains(sensitiveKeys, a.Key) {
		return slog.String(a.Key, "[REDACTED]")
	}
	// redact embedded passwords in url string values
	if val, ok := a.Value.Any().(string); ok {
		if u, err := url.Parse(val); err == nil && u.User != nil {
			u.User = url.User("[REDACTED]")
			a = slog.String(a.Key, u.String())
		}
	}

	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		if me, ok := errors.AsType[multiError](err); ok {
			var errAttrs []slog.Attr
			for i, e := range me.Unwrap() {
				errAttrs = append(errAttrs, slog.GroupAttrs(
					fmt.Sprintf("error_%d", i+1), errorAttrs(e)...,
				))
			}
			return slog.GroupAttrs("errors", errAttrs...)
		}

		return slog.GroupAttrs("error", errorAttrs(err)...)
	}
	return a
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	logFile := os.Getenv("LINKO_LOG_FILE")
	logger, closeLogger, err := initializeLogger(logFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to initialize the logger: %v\n", err)
		return 1
	}
	defer func() {
		if err := closeLogger(); err != nil {
			fmt.Fprintf(os.Stderr, "failed to close logger: %v\n", err)
		}
	}()

	// query host runtime details
	hostname, _ := os.Hostname()

	// decorate the base logger with build and runtime instance attributes
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", os.Getenv("ENV")),
		slog.String("hostname", hostname),
	)

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error("failed to create store", "error", err)
		return 1
	}
	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	logger.Debug("Linko is shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error("failed to shutdown server", "error", err)
		return 1
	}
	if serverErr != nil {
		logger.Error("server error", "error", serverErr)
		return 1
	}
	return 0
}
