package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required; bind a managed SQLite service in Data services, then deploy again")
	}
	db, err := sql.Open("sqlite3", databaseURL)
	if err != nil {
		log.Fatalf("open SQLite database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatalf("connect to SQLite database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS counters (id INTEGER PRIMARY KEY, val INTEGER NOT NULL);`); err != nil {
		log.Fatalf("initialize SQLite database: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "sqlite demo app")
	})
	mux.HandleFunc("/counter", func(w http.ResponseWriter, r *http.Request) {
		var value int
		err := db.QueryRowContext(r.Context(), `
			INSERT INTO counters (id, val) VALUES (1, 1)
			ON CONFLICT (id) DO UPDATE SET val = counters.val + 1
			RETURNING val;
		`).Scan(&value)
		if err != nil {
			http.Error(w, fmt.Sprintf("database query failed: %v", err), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "%d\n", value)
	})

	log.Printf("sqlite-app listening on 127.0.0.1:%s", port)
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, mux))
}
