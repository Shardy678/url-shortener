package storage

import (
	"context"

	"url-shortener/internal/metrics"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Repo struct{ Pool *pgxpool.Pool }

var ErrConflict = pgx.ErrNoRows

func New(pool *pgxpool.Pool) *Repo { return &Repo{Pool: pool} }

var ErrSlugConflict = fmtError("slug already exists")

type fmtError string

func (e fmtError) Error() string { return string(e) }

func (r *Repo) Create(ctx context.Context, code, url string) error {
	metrics.DBOpsTotal.WithLabelValues("create_url").Inc()
	ct, err := r.Pool.Exec(ctx, "INSERT INTO urls (code, target_url) VALUES ($1, $2) ON CONFLICT (code) DO NOTHING", code, url)
	if err != nil {
		metrics.DBErrorsTotal.WithLabelValues("create_url").Inc()
		return err
	}
	if ct.RowsAffected() == 0 {
		return ErrSlugConflict
	}
	return nil
}

func (r *Repo) Get(ctx context.Context, code string) (string, bool, error) {
	metrics.DBOpsTotal.WithLabelValues("get_url").Inc()
	var url string
	err := r.Pool.QueryRow(ctx, "SELECT target_url FROM urls WHERE code=$1", code).Scan(&url)
	if err != nil {
		if err == pgx.ErrNoRows {
			return "", false, nil
		}
		metrics.DBErrorsTotal.WithLabelValues("get_url").Inc()
		return "", false, err
	}
	return url, true, nil
}
