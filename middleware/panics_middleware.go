package middleware

import (
	"github.com/gorilla/mux"
	"net/http"
	logging2 "proxy/util/logging"
)

func PanicsHandling() mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if err := recover(); err != nil {
					logging2.ERROR.Printf("[%v] proxy panic: %v", r.RemoteAddr, err)
					w.WriteHeader(http.StatusBadGateway)
					_, _ = w.Write([]byte(err.(error).Error()))
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}
