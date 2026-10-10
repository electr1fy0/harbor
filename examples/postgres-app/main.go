package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"

	_ "github.com/lib/pq"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	dbURL := os.Getenv("DATABASE_URL")
	log.Printf("postgres-app starting with DATABASE_URL set=%v", dbURL != "")

	var db *sql.DB
	if dbURL != "" {
		var err error
		db, err = sql.Open("postgres", dbURL)
		if err != nil {
			log.Printf("warning: sql.Open failed: %v", err)
		} else {
			defer db.Close()
			if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS counters (id INT PRIMARY KEY, val INT);`); err != nil {
				log.Printf("warning: init counters table: %v", err)
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintln(w, "ok")
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintln(w, "postgres demo app")
	})

	mux.HandleFunc("/counter", func(w http.ResponseWriter, _ *http.Request) {
		if db == nil {
			http.Error(w, "DATABASE_URL not configured", http.StatusInternalServerError)
			return
		}

		// Ensure table exists
		_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS counters (id INT PRIMARY KEY, val INT);`)

		var val int
		err := db.QueryRow(`
			INSERT INTO counters (id, val) VALUES (1, 1)
			ON CONFLICT (id) DO UPDATE SET val = counters.val + 1
			RETURNING val;
		`).Scan(&val)
		if err != nil {
			http.Error(w, fmt.Sprintf("database query failed: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "text/plain")
		_, _ = fmt.Fprintf(w, "%d\n", val)
	})

	log.Printf("postgres-app listening on 127.0.0.1:%s", port)
	log.Fatal(http.ListenAndServe("127.0.0.1:"+port, mux))
}
