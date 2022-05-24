package main

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
)

const maxPktSize = 1024

func readUnixConn(conn *net.UnixConn, msgs chan []byte) {
	for {
		msg := make([]byte, maxPktSize)
		nread, err := conn.Read(msg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Failed to read from unix socket: %v\n", err)
			return
		}

		msgs <- msg[:nread]
	}
}

type unixProxy struct {
	local, remote *net.UnixConn
}

func newProxy(src, dst string) (*unixProxy, error) {
	os.Remove(src)

	// start listening
	local, err := net.ListenUnixgram("unixgram", &net.UnixAddr{
		Name: src,
		Net:  "unixgram",
	})

	if err != nil {
		return nil, err
	}

	remote, err := net.DialUnix("unixgram", nil, &net.UnixAddr{
		Name: dst,
		Net:  "unixgram",
	})

	if err != nil {
		return nil, err
	}

	return &unixProxy{
		local:  local,
		remote: remote,
	}, nil
}

func (p *unixProxy) run(cancel chan struct{}) {
	msgs := make(chan []byte)

	go readUnixConn(p.local, msgs)

	for {
		select {
		case msg := <-msgs:
			p.remote.Write(msg)

		case <-cancel:
			p.local.Close()
			p.remote.Close()
			return
		}
	}
}

func forkExec(argv []string) (*os.Process, error) {
	return os.StartProcess(argv[0], argv, &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})
}

func main() {
	reqd_args := 3
	arg := 1
	unset := false
	if len(os.Args) > arg && os.Args[arg] == "--unset" {
		unset = true
		arg++
		reqd_args++
	}
	if len(os.Args) < reqd_args {
		fmt.Fprintf(os.Stderr, "Usage: %s [--unset] proxy-socket cmd ...\n", os.Args[0])
		os.Exit(1)
	}

	sdSock := os.Getenv("NOTIFY_SOCKET")
	if sdSock == "" {
		fmt.Fprintf(os.Stderr, "NOTIFY_SOCKET environment variable not set\n")
		os.Exit(1)
	}

	proxySock := os.Args[arg]

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGCHLD)

	if unset {
		// unset NOTIFY_SOCKET
		os.Unsetenv("NOTIFY_SOCKET")
	} else {
		// replace NOTIFY_SOCKET with the proxy socket
		os.Setenv("NOTIFY_SOCKET", proxySock)
	}

	proxy, err := newProxy(proxySock, sdSock)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating proxy: %v\n", err)
		os.Exit(1)
	}

	// fork/exec
	proc, err := forkExec(os.Args[arg+1:len(os.Args)])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error executing command: %v\n", err)
		os.Exit(1)
	}

	// proxy the unixgram messages
	cancel := make(chan struct{})
	go proxy.run(cancel)

	for {
		sig := <-sigs

		switch sig {
		case syscall.SIGINT, syscall.SIGTERM:
			// propogate to child
			proc.Signal(sig)

		case syscall.SIGCHLD:
			ps, err := proc.Wait()
			if err != nil {
				fmt.Fprintf(os.Stderr, "waitpid failed: %v\n", err)
				os.Exit(1)
			}

			close(cancel)

			ec := ps.Sys().(syscall.WaitStatus).ExitStatus()
			os.Exit(ec)
		}
	}
}
