package exec

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// startSSHServer runs an in-process SSH server that mimics OrbStack's
// multiplexer: it authenticates "<user>@<machine>" users against the given
// public keys, executes a small set of fake commands, and serves SFTP
// against the test process's real filesystem.
//
// Fake commands:
//
//	echo <text>     -> <text>\n on stdout, exit 0
//	fail            -> "went wrong\n" on stderr, exit 3
//	whoami          -> the SSH user on stdout, exit 0
//	sleep           -> blocks until the connection drops
//	printenv <name> -> prints an injected env var
//	run '<path>'    -> contents of local file <path> on stdout, exit 0
//	rm -f '<path>'  -> removes local file <path>, exit 0
func startSSHServer(t *testing.T, authorized map[string]ssh.PublicKey) string {
	t.Helper()

	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			want, ok := authorized[conn.User()]
			if !ok || !bytes.Equal(want.Marshal(), key.Marshal()) {
				return nil, os.ErrPermission
			}
			return nil, nil
		},
	}
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AddHostKey(hostSigner)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveConn(conn, cfg)
		}
	}()
	return ln.Addr().String()
}

func serveConn(conn net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)

	for newCh := range chans {
		if newCh.ChannelType() != "session" {
			newCh.Reject(ssh.UnknownChannelType, "only session channels")
			continue
		}
		ch, chReqs, err := newCh.Accept()
		if err != nil {
			continue
		}
		go serveSession(sconn.User(), ch, chReqs)
	}
}

func serveSession(user string, ch ssh.Channel, reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "exec":
			var payload struct{ Command string }
			ssh.Unmarshal(req.Payload, &payload)
			req.Reply(true, nil)
			runFakeCommand(user, payload.Command, ch)
			return
		case "subsystem":
			var payload struct{ Name string }
			ssh.Unmarshal(req.Payload, &payload)
			if payload.Name != "sftp" {
				req.Reply(false, nil)
				continue
			}
			req.Reply(true, nil)
			srv, err := sftp.NewServer(ch)
			if err == nil {
				srv.Serve()
			}
			ch.Close()
			return
		default:
			req.Reply(false, nil)
		}
	}
}

func runFakeCommand(user, command string, ch ssh.Channel) {
	runFakeCommandWithEnv(user, command, ch, nil)
}

func runFakeCommandWithEnv(user, command string, ch ssh.Channel, env map[string]string) {
	exit := func(code uint32) {
		ch.SendRequest("exit-status", false, ssh.Marshal(&struct{ Status uint32 }{code}))
		ch.Close()
	}
	unquote := func(s string) string {
		s = strings.TrimSpace(s)
		if strings.HasPrefix(s, "'") && strings.HasSuffix(s, "'") {
			s = strings.TrimPrefix(strings.TrimSuffix(s, "'"), "'")
		}
		return strings.ReplaceAll(s, `'\''`, `'`)
	}

	switch {
	case strings.HasPrefix(command, "echo "):
		io.WriteString(ch, strings.TrimPrefix(command, "echo ")+"\n")
		exit(0)
	case command == "fail":
		io.WriteString(ch.Stderr(), "went wrong\n")
		exit(3)
	case command == "whoami":
		io.WriteString(ch, user+"\n")
		exit(0)
	case strings.HasPrefix(command, "printenv "):
		name := strings.TrimSpace(strings.TrimPrefix(command, "printenv "))
		if val, ok := env[name]; ok {
			io.WriteString(ch, val+"\n")
		}
		exit(0)
	case command == "sleep":
		// Block until the client tears the connection down: writes start
		// failing once the connection is gone.
		for {
			if _, err := io.WriteString(ch, "."); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	case command == "sh -s":
		data, err := io.ReadAll(ch)
		if err != nil {
			io.WriteString(ch.Stderr(), err.Error()+"\n")
			exit(1)
			return
		}
		wrappedCommand, wrappedEnv := parseEnvWrapper(string(data), unquote)
		runFakeCommandWithEnv(user, wrappedCommand, ch, wrappedEnv)
	case strings.HasPrefix(command, "run "), strings.HasPrefix(command, "sh "):
		// "run <path>" / "sh <path>": read the uploaded script and echo it back
		// (the latter lets tests use a real allowlisted interpreter name).
		_, path, _ := strings.Cut(command, " ")
		data, err := os.ReadFile(unquote(path))
		if err != nil {
			io.WriteString(ch.Stderr(), err.Error()+"\n")
			exit(1)
			return
		}
		if handled := writeScriptOutput(string(data), env, ch); !handled {
			ch.Write(data)
		}
		exit(0)
	case strings.HasPrefix(command, "rm -f "):
		os.Remove(unquote(strings.TrimPrefix(command, "rm -f ")))
		exit(0)
	default:
		io.WriteString(ch.Stderr(), "unknown fake command: "+command+"\n")
		exit(127)
	}
}

func writeScriptOutput(script string, env map[string]string, ch ssh.Channel) bool {
	handled := false
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "printenv ") {
			handled = true
			name := strings.TrimSpace(strings.TrimPrefix(line, "printenv "))
			if val, ok := env[name]; ok {
				io.WriteString(ch, val+"\n")
			}
		}
	}
	return handled
}

func parseEnvWrapper(script string, unquote func(string) string) (string, map[string]string) {
	env := map[string]string{}
	command := ""
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "export "):
			pair := strings.TrimPrefix(line, "export ")
			name, value, ok := strings.Cut(pair, "=")
			if ok {
				env[name] = unquote(value)
			}
		case strings.HasPrefix(line, "exec sh -c "):
			command = unquote(strings.TrimPrefix(line, "exec sh -c "))
		}
	}
	return command, env
}
