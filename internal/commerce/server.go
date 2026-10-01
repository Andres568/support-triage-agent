package commerce

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
)

// The API is read-only on purpose: there is no endpoint an agent could use to
// refund, cancel or edit anything. What does not exist cannot be misused.

type querier interface {
	OrderForCustomer(ctx context.Context, orderNumber, customerEmail string) (Order, error)
	SearchPolicies(ctx context.Context, query string, limit int) ([]Policy, error)
}

// Inputs are validated here too, not only by the tools: the API is the
// boundary, and a bad number or a huge query should not reach the database.
var orderNumberRE = regexp.MustCompile(`^ORD-[0-9]{6}$`)

const maxQueryBytes = 200

// NewHandler routes:
//
//	GET /v1/orders/{number}  + X-Customer-Email   order, only if it belongs to that customer
//	GET /v1/policies?q=...                        top 3 matching policies
//	GET /healthz
func NewHandler(s querier, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/orders/{number}", func(w http.ResponseWriter, r *http.Request) {
		email := strings.TrimSpace(r.Header.Get(CustomerEmailHeader))
		if email == "" {
			writeError(w, http.StatusBadRequest, CustomerEmailHeader+" header is required")
			return
		}
		num := r.PathValue("number")
		if !orderNumberRE.MatchString(num) {
			writeError(w, http.StatusBadRequest, "order number must look like ORD-123456")
			return
		}
		o, err := s.OrderForCustomer(r.Context(), num, email)
		switch {
		case errors.Is(err, ErrNotFound):
			writeError(w, http.StatusNotFound, "order not found for this customer")
		case err != nil:
			log.ErrorContext(r.Context(), "get order", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
		default:
			writeJSON(w, http.StatusOK, o)
		}
	})

	mux.HandleFunc("GET /v1/policies", func(w http.ResponseWriter, r *http.Request) {
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		if q == "" {
			writeError(w, http.StatusBadRequest, "q is required")
			return
		}
		if len(q) > maxQueryBytes {
			writeError(w, http.StatusBadRequest, "q is too long")
			return
		}
		ps, err := s.SearchPolicies(r.Context(), q, 3)
		if err != nil {
			log.ErrorContext(r.Context(), "search policies", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"policies": ps})
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})

	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
