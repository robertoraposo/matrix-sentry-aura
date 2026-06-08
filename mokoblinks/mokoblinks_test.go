package mokoblinks

import "testing"

func TestDisabledClientIsNoOp(t *testing.T) {
	c := &Client{} // zero value: disabled, send==nil
	c.Info("should not buffer or panic", map[string]string{"k": "v"})
	c.Flush()
	if len(c.buf) != 0 {
		t.Fatalf("disabled client buffered %d lines, want 0", len(c.buf))
	}
}

func TestBatchesAndFlushes(t *testing.T) {
	var sent [][]Line
	c := &Client{enabled: true, app: "matrix-sentry", batchSize: 100}
	c.send = func(lines []Line) { sent = append(sent, lines) }

	c.Info("opened journal", map[string]string{"dir": "/tmp/j"})
	c.Warn("recovered torn tail", nil)
	if len(sent) != 0 {
		t.Fatalf("flushed before batch full / explicit flush: %d sends", len(sent))
	}

	c.Flush()
	if len(sent) != 1 || len(sent[0]) != 2 {
		t.Fatalf("Flush sent %d batches; first has %d lines (want 1 batch, 2 lines)", len(sent), len(sent[0]))
	}
	if sent[0][0].Level != "info" || sent[0][0].App != "matrix-sentry" || sent[0][0].Line != "opened journal" {
		t.Fatalf("line not framed correctly: %+v", sent[0][0])
	}
	if sent[0][1].Level != "warn" {
		t.Fatalf("second line level = %q, want warn", sent[0][1].Level)
	}

	// Buffer is cleared after flush.
	c.Flush()
	if len(sent) != 1 {
		t.Fatalf("empty flush sent again: %d batches", len(sent))
	}
}
