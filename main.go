package main

import (
	"boot.dev/linko/internal/linkoerr"
	"boot.dev/linko/internal/store"
	"context"
	"errors"
	"flag"
	"fmt"
	pkgerr "github.com/pkg/errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"
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

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {
	if logFile != "" {
		file, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, err
		}
		debugHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level:       slog.LevelDebug,
			ReplaceAttr: replaceAttr,
		})
		infoHandler := slog.NewJSONHandler(file, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		})
		multiHandler := slog.NewMultiHandler(debugHandler, infoHandler)
		return slog.New(multiHandler), func() error {
			return file.Close()
		}, nil
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{ReplaceAttr: replaceAttr})), func() error { return nil }, nil
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		attrs := []slog.Attr{
			slog.String("message", err.Error()),
		}

		attrs = append(attrs, linkoerr.Attrs(err)...)

		if tracer, ok := errors.AsType[stackTracer](err); ok {
			attrs = append(attrs, slog.String("stack_trace", fmt.Sprintf("%+v", tracer.StackTrace())))
		}
		return slog.GroupAttrs("error", attrs...)
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
