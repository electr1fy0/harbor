package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"paas/internal/service"
)

func runExportDecrypt(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fail := func(err error) int { fmt.Fprintf(stderr, "paas export-decrypt: %v\n", err); return 1 }
	if len(args) != 3 || args[2] != "--passphrase-stdin" {
		return reportUsageError(stderr, errors.New("export-decrypt requires INPUT OUTPUT --passphrase-stdin"))
	}
	line, err := bufio.NewReader(io.LimitReader(stdin, 1026)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fail(errors.New("could not read passphrase from stdin"))
	}
	passphrase := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
	if len(passphrase) < 12 || len(passphrase) > 1024 {
		return fail(errors.New("passphrase must contain 12 to 1024 bytes"))
	}
	input, err := os.Open(args[0])
	if err != nil {
		return fail(errors.New("could not open encrypted archive"))
	}
	defer input.Close()
	const maxEncrypted = 48<<20 + 72
	encrypted, err := io.ReadAll(io.LimitReader(input, maxEncrypted+1))
	if err != nil || len(encrypted) > maxEncrypted {
		return fail(errors.New("encrypted archive exceeds the supported size or could not be read"))
	}
	plain, err := service.DecryptHarborExport(passphrase, encrypted)
	if err != nil {
		return fail(err)
	}
	output, err := os.OpenFile(args[1], os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fail(errors.New("output must be a new writable file; existing files are never overwritten"))
	}
	good := false
	defer func() {
		output.Close()
		if !good {
			os.Remove(args[1])
		}
	}()
	if err = output.Chmod(0600); err != nil {
		return fail(errors.New("could not secure decrypted archive"))
	}
	if _, err = output.Write(plain); err != nil {
		return fail(errors.New("could not write decrypted archive"))
	}
	if err = output.Sync(); err != nil {
		return fail(errors.New("could not persist decrypted archive"))
	}
	if err = output.Close(); err != nil {
		return fail(errors.New("could not close decrypted archive"))
	}
	good = true
	fmt.Fprintf(stdout, "Decrypted archive: %s\n", args[1])
	return 0
}
