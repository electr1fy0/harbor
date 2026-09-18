// Command paas is the local CLI client. Commands stay thin and talk to paasd
// through the Unix-socket HTTP protocol.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"paas/internal/daemon"
)

func main() {
	socket, command, err := parseArgs(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		fmt.Fprintln(os.Stderr, "usage: paas [-socket PATH] ping")
		os.Exit(2)
	}

	switch command {
	case "ping":
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		result, err := daemon.Ping(ctx, socket)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(result)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", command)
		os.Exit(2)
	}
}

func parseArgs(args []string) (socket string, command string, err error) {
	socket = os.Getenv("PAASD_SOCKET")
	if socket == "" {
		socket = daemon.DefaultSocketPath
	}

	for index := 0; index < len(args); index++ {
		switch {
		case args[index] == "-socket" || args[index] == "--socket":
			if index+1 >= len(args) || args[index+1] == "" {
				return "", "", fmt.Errorf("%s requires a path", args[index])
			}
			socket = args[index+1]
			index++
		case strings.HasPrefix(args[index], "-socket="):
			socket = strings.TrimPrefix(args[index], "-socket=")
			if socket == "" {
				return "", "", fmt.Errorf("-socket requires a path")
			}
		case strings.HasPrefix(args[index], "--socket="):
			socket = strings.TrimPrefix(args[index], "--socket=")
			if socket == "" {
				return "", "", fmt.Errorf("--socket requires a path")
			}
		case strings.HasPrefix(args[index], "-"):
			return "", "", fmt.Errorf("unknown option %q", args[index])
		case command == "":
			command = args[index]
		default:
			return "", "", fmt.Errorf("unexpected argument %q", args[index])
		}
	}

	if command == "" {
		return "", "", fmt.Errorf("expected exactly one command")
	}
	return socket, command, nil
}
