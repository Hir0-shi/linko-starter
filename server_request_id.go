package main

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

func requestID() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")

			if id == "" {
				b := make([]byte, 16)
				rand.Read(b)
				id = hex.EncodeToString(b)
			}

			w.Header().Set("X-Request-ID", id)

			next.ServeHTTP(w, r)
		})
	}
}
