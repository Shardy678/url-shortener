package analytics

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"url-shortener/internal/metrics"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Worker struct {
	buf        []EventWithCode
	flushEvery time.Duration
	maxBatch   int
	ch         chan EventWithCode
	stop       chan struct{}
	stopped    chan struct{}
	pool       *pgxpool.Pool
}

func NewWorker(pool *pgxpool.Pool, bufSize, maxBatch int, flushEvery time.Duration) *Worker {
	return &Worker{
		buf:        make([]EventWithCode, 0, maxBatch),
		flushEvery: flushEvery,
		maxBatch:   maxBatch,
		ch:         make(chan EventWithCode, bufSize),
		stop:       make(chan struct{}),
		stopped:    make(chan struct{}),
		pool:       pool,
	}
}

func (w *Worker) Start() {
	go func() {
		defer close(w.stopped)
		ticker := time.NewTicker(w.flushEvery)
		defer ticker.Stop()
		for {
			metrics.ClickQueueDepth.Set(float64(len(w.ch)))
			select {
			case ev := <-w.ch:
				w.buf = append(w.buf, ev)
				if len(w.buf) >= w.maxBatch {
					w.flush(context.Background())
				}
			case <-ticker.C:
				if len(w.buf) > 0 {
					w.flush(context.Background())
				}
			case <-w.stop:
				if len(w.buf) > 0 {
					w.flush(context.Background())
				}
				return
			}
		}
	}()
}

func (w *Worker) Stop() { close(w.stop); <-w.stopped }

func (w *Worker) Enqueue(ev EventWithCode) {
	select {
	case w.ch <- ev:
	default:
	}
	metrics.ClickQueueDepth.Set(float64(len(w.ch)))
}

func (w *Worker) flush(ctx context.Context) {
	if len(w.buf) == 0 {
		return
	}
	metrics.ClickFlushBatchSize.Observe(float64(len(w.buf)))
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	sb := strings.Builder{}
	sb.WriteString("INSERT INTO clicks(code, ts, ip, user_agent, device, os, browser) VALUES ")
	args := make([]any, 0, len(w.buf)*7)
	for i, it := range w.buf {
		if i > 0 {
			sb.WriteByte(',')
		}
		idx := i * 7
		sb.WriteString(fmt.Sprintf("($%d,$%d,$%d,$%d,$%d,$%d,$%d)", idx+1, idx+2, idx+3, idx+4, idx+5, idx+6, idx+7))
		args = append(args, it.Code, it.E.Timestamp, it.E.IP, it.E.UserAgent, it.E.Device, it.E.OS, it.E.Browser)
	}
	metrics.DBOpsTotal.WithLabelValues("clicks_insert").Inc()
	if _, err := w.pool.Exec(ctx, sb.String(), args...); err != nil {
		metrics.DBErrorsTotal.WithLabelValues("clicks_insert").Inc()
		log.Printf("click insert error: %v", err)
	}
	w.buf = w.buf[:0]
}
