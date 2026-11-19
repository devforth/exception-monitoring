// Package control is the running server's local socket for commands (backup).
package control

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SocketName is the socket in DATA_DIR: only the server's own user (the container) reaches it.
const SocketName = "control.sock"

// SocketPath is the socket of the server with this data directory.
func SocketPath(dataDir string) string { return filepath.Join(dataDir, SocketName) }

// BackupFunc writes a backup into target, reporting progress lines; it returns a summary.
type BackupFunc func(ctx context.Context, target string, progress func(string)) (string, error)

// Server answers commands on the socket.
type Server struct {
	listener net.Listener
	path     string
	logger   *slog.Logger
	backup   BackupFunc
	running  atomic.Bool // one backup at a time

	ctx         context.Context
	cancel      context.CancelFunc
	connections sync.WaitGroup
}

// Protocol: one request line, then "progress <text>" lines and one "ok <summary>" or "error <message>".
const (
	requestLimit   = 4096
	requestTimeout = 10 * time.Second
)

// Listen opens the socket of dataDir, replacing a stale one; the database lock keeps a second server out.
func Listen(dataDir string, logger *slog.Logger, backup BackupFunc) (*Server, error) {
	path := SocketPath(dataDir)

	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}

		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}

	// Made under another name and renamed once it is 0600, so it is never reachable with the umask's mode.
	building := path + ".new"
	_ = os.Remove(building)

	listener, err := net.Listen("unix", building)

	if err != nil {
		return nil, err
	}

	listener.(*net.UnixListener).SetUnlinkOnClose(false)

	if err := errors.Join(os.Chmod(building, 0o600), os.Rename(building, path)); err != nil {
		_ = listener.Close()
		_ = os.Remove(building)

		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	server := &Server{listener: listener, path: path, logger: logger, backup: backup, ctx: ctx, cancel: cancel}

	server.connections.Go(server.serve)

	return server, nil
}

// Close stops listening, cancels a running backup, waits for it and removes the socket.
func (server *Server) Close() error {
	server.cancel()
	err := server.listener.Close()
	server.connections.Wait()

	if removeErr := os.Remove(server.path); !errors.Is(removeErr, fs.ErrNotExist) {
		err = errors.Join(err, removeErr)
	}

	return err
}

func (server *Server) serve() {
	for {
		conn, err := server.listener.Accept()

		if err != nil {
			if server.ctx.Err() == nil {
				server.logger.Warn("control socket", "err", err)
			}

			return
		}

		server.connections.Go(func() {
			defer func() { _ = conn.Close() }()

			server.handle(conn)
		})
	}
}

func (server *Server) handle(conn net.Conn) {
	// Close ends every connection.
	stop := context.AfterFunc(server.ctx, func() { _ = conn.Close() })

	defer stop()

	_ = conn.SetReadDeadline(time.Now().Add(requestTimeout))
	line, err := bufio.NewReader(io.LimitReader(conn, requestLimit)).ReadString('\n')

	if err != nil {
		return
	}

	command, target, _ := strings.Cut(strings.TrimSpace(line), " ")

	switch {
	case command != "backup":
		reply(conn, "error", fmt.Sprintf("unknown command %q", command))

		return
	case !filepath.IsAbs(target):
		reply(conn, "error", fmt.Sprintf("the backup path %q is not absolute", target))

		return
	case !server.running.CompareAndSwap(false, true):
		reply(conn, "error", "another backup is running")

		return
	}

	defer server.running.Store(false)

	ctx, cancel := context.WithCancel(server.ctx)

	defer cancel()

	// The client going away (Ctrl+C) cancels the backup.
	_ = conn.SetReadDeadline(time.Time{})

	go func() {
		_, _ = conn.Read(make([]byte, 1))

		cancel()
	}()

	summary, err := server.backup(ctx, target, func(text string) {
		if !reply(conn, "progress", text) {
			cancel()
		}
	})

	if err != nil {
		reply(conn, "error", err.Error())

		return
	}

	reply(conn, "ok", summary)
}

// reply writes one line; false when the client is gone.
func reply(conn net.Conn, kind, text string) bool {
	_, err := fmt.Fprintf(conn, "%s %s\n", kind, strings.ReplaceAll(text, "\n", " "))

	return err == nil
}

// ErrNoServer: no server answers on the socket.
var ErrNoServer = errors.New("no server answers on the control socket")

// Backup asks the server of dataDir to write a backup into target; progress lines go to out.
func Backup(ctx context.Context, dataDir, target string, out io.Writer) (string, error) {
	var dialer net.Dialer

	dialCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	conn, err := dialer.DialContext(dialCtx, "unix", SocketPath(dataDir))

	cancel()

	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrNoServer, err)
	}

	defer func() { _ = conn.Close() }()

	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })

	defer stop()

	if _, err := fmt.Fprintf(conn, "backup %s\n", target); err != nil {
		return "", err
	}

	scanner := bufio.NewScanner(conn)

	for scanner.Scan() {
		kind, text, _ := strings.Cut(scanner.Text(), " ")

		switch kind {
		case "progress":
			_, _ = fmt.Fprintln(out, text)
		case "ok":
			return text, nil
		case "error":
			return "", errors.New(text)
		}
	}

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	return "", fmt.Errorf("the server closed the connection before the backup finished: %w", errors.Join(scanner.Err(), io.ErrUnexpectedEOF))
}
