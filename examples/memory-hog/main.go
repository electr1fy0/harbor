package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
)

func main() {
	megabytes := envInt("MEMORY_HOG_MB", 256)
	if megabytes < 1 {
		megabytes = 1
	}

	chunks := make([][]byte, 0, megabytes)
	for i := 0; i < megabytes; i++ {
		chunk := make([]byte, 1024*1024)
		for page := 0; page < len(chunk); page += 4096 {
			chunk[page] = byte(i)
		}
		chunks = append(chunks, chunk)
	}

	runtime.KeepAlive(chunks)
	fmt.Printf("allocated %d MiB; holding memory\n", megabytes)
	for {
		time.Sleep(time.Minute)
	}
}

func envInt(name string, fallback int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil {
		return fallback
	}
	return value
}
