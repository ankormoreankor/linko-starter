package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"boot.dev/linko/internal/build"
	"boot.dev/linko/internal/linkoerr"

	"boot.dev/linko/internal/store"
	pkgerr "github.com/pkg/errors"
)

type closeFunc func() error

func makeCloseFunc(writer *bufio.Writer, file *os.File) closeFunc {
	return func() error {
		var err error
		err = writer.Flush()
		if err != nil {
			return err
		}
		err = file.Close()
		if err != nil {
			return err
		}
		return nil
	}
}

type stackTracer interface {
	error
	StackTrace() pkgerr.StackTrace
}

type multiError interface {
	error
	Unwrap() []error
}

func errorAttrs(err error) []slog.Attr {
	attrs := []slog.Attr{
		{
			Key:   "message",
			Value: slog.StringValue(err.Error()),
		},
	}

	attrs = append(attrs, linkoerr.Attrs(err)...)

	if stackErr, ok := errors.AsType[stackTracer](err); ok {
		attrs = append(attrs, slog.Attr{
			Key:   "stack_trace",
			Value: slog.StringValue(fmt.Sprintf("%+v", stackErr.StackTrace())),
		})
	}
	return attrs
}

func replaceAttr(groups []string, a slog.Attr) slog.Attr {
	if a.Key == "error" {
		err, ok := a.Value.Any().(error)
		if !ok {
			return a
		}

		var attrs []slog.Attr
		if mErr, ok := errors.AsType[multiError](err); ok {
			for i, e := range mErr.Unwrap() {
				extraAttrs := errorAttrs(e)
				attrs = append(attrs, slog.GroupAttrs(fmt.Sprintf("error_%d", i+1), extraAttrs...))
			}
			return slog.GroupAttrs("errors", attrs...)
		}

		attrs = errorAttrs(err)
		return slog.GroupAttrs("error", attrs...)

	}
	return a
}

func initializeLogger(logFile string) (*slog.Logger, closeFunc, error) {

	debugHandler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level:       slog.LevelDebug,
		ReplaceAttr: replaceAttr,
	})

	if logFile != "" {
		file, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, err
		}

		bufferedFile := bufio.NewWriterSize(file, 8192)
		close := makeCloseFunc(bufferedFile, file)

		infoHandler := slog.NewJSONHandler(bufferedFile, &slog.HandlerOptions{
			Level:       slog.LevelInfo,
			ReplaceAttr: replaceAttr,
		})

		return slog.New(slog.NewMultiHandler(
			debugHandler,
			infoHandler,
		)), close, nil
	}

	return slog.New(debugHandler), func() error {
		return nil
	}, nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	httpPort := flag.Int("port", 8899, "port to listen on")
	dataDir := flag.String("data", "./data", "directory to store data")
	flag.Parse()

	status := run(ctx, cancel, *httpPort, *dataDir)
	cancel()
	os.Exit(status)
}

func run(ctx context.Context, cancel context.CancelFunc, httpPort int, dataDir string) int {
	env := os.Getenv("ENV")
	hostname, _ := os.Hostname()
	logger, closeLogger, err := initializeLogger(os.Getenv("LINKO_LOG_FILE"))
	logger = logger.With(
		slog.String("git_sha", build.GitSHA),
		slog.String("build_time", build.BuildTime),
		slog.String("env", env),
		slog.String("hostname", hostname),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to open log file: %v", err)
		return 1
	}

	defer func() {
		err := closeLogger()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to close logger: %v", err)
		}
	}()

	st, err := store.New(dataDir, logger)
	if err != nil {
		logger.Error(fmt.Sprintf("failed to create store: %v", err))
		return 1
	}

	s := newServer(*st, httpPort, cancel, logger)
	var serverErr error
	go func() {
		serverErr = s.start()
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.shutdown(shutdownCtx); err != nil {
		logger.Error(fmt.Sprintf("failed to shutdown server: %v", err))
		return 1
	}
	if serverErr != nil {
		logger.Error(fmt.Sprintf("server error: %v", serverErr))
		return 1
	}
	return 0
}
