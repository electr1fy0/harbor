package main

import (
	"testing"
)

func TestParseArgsSupportsSocketBeforeOrAfterCommand(t *testing.T) {
	for _, test := range []struct {
		name   string
		args   []string
		socket string
	}{
		{name: "before", args: []string{"-socket", "/tmp/one.sock", "ping"}, socket: "/tmp/one.sock"},
		{name: "after", args: []string{"ping", "--socket", "/tmp/two.sock"}, socket: "/tmp/two.sock"},
		{name: "equals", args: []string{"--socket=/tmp/three.sock", "ping"}, socket: "/tmp/three.sock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			socket, command, err := parseArgs(test.args)
			if err != nil {
				t.Fatal(err)
			}
			if socket != test.socket || command != "ping" {
				t.Fatalf("parseArgs() = (%q, %q), want (%q, ping)", socket, command, test.socket)
			}
		})
	}
}

func TestParseArgsUsesEnvironmentSocket(t *testing.T) {
	const socket = "/tmp/from-env.sock"
	t.Setenv("PAASD_SOCKET", socket)
	gotSocket, command, err := parseArgs([]string{"ping"})
	if err != nil {
		t.Fatal(err)
	}
	if gotSocket != socket || command != "ping" {
		t.Fatalf("parseArgs() = (%q, %q), want (%q, ping)", gotSocket, command, socket)
	}
}

func TestParseArgsRejectsUnknownOption(t *testing.T) {
	t.Setenv("PAASD_SOCKET", "")
	if _, _, err := parseArgs([]string{"--unknown", "ping"}); err == nil {
		t.Fatal("parseArgs unexpectedly accepted unknown option")
	}
}
