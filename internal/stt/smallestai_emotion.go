package stt

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"
)

// SmallestaiEmotion streams a call recording (linear16, 8 kHz) to smallestai pulse-2
// and returns its emotion scores, averaged over the finals and weighted by
// their duration. It judges how the audio sounds, not what it says.
func SmallestaiEmotion(ctx context.Context, recording, apiKey string) (map[string]float64, error) {
	pcm, err := os.ReadFile(recording)
	if err != nil {
		return nil, err
	}

	q := url.Values{"model": {"pulse-2"}, "encoding": {"linear16"}, "sample_rate": {"8000"}, "language": {"en"}}
	hdr := http.Header{"Authorization": {"Bearer " + apiKey}}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, "wss://api.smallest.ai/waves/v1/stt/live?"+q.Encode(), hdr)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("smallestai dial: %w (http %d)", err, resp.StatusCode)
		}
		return nil, fmt.Errorf("smallestai dial: %w", err)
	}
	defer conn.Close()

	sum := map[string]float64{}
	var weight float64
	var seen []string
	done := make(chan error, 1)
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				done <- nil
				return
			}
			var m struct {
				Type     string  `json:"type"`
				IsFinal  bool    `json:"is_final"`
				IsLast   bool    `json:"is_last"`
				Duration float64 `json:"duration"`
				Emotion  *struct {
					Scores map[string]float64 `json:"scores"`
				} `json:"emotion"`
			}
			if len(seen) < 8 {
				seen = append(seen, string(data[:min(len(data), 300)]))
			}
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			if m.Type == "transcription" && m.IsFinal && m.Emotion != nil {
				w := m.Duration
				if w <= 0 {
					w = 1
				}
				for k, v := range m.Emotion.Scores {
					sum[k] += v * w
				}
				weight += w
			}
			if m.IsLast {
				done <- nil
				return
			}
		}
	}()

	// real-time pacing so the vendor's endpointing sees real silence
	const frame = 320 // 20 ms of 8 kHz linear16
	for i := 0; i < len(pcm); i += frame {
		end := min(i+frame, len(pcm))
		if err := conn.WriteMessage(websocket.BinaryMessage, pcm[i:end]); err != nil {
			return nil, fmt.Errorf("smallestai write: %w", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"close_stream"}`))
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if weight == 0 {
		return nil, fmt.Errorf("smallestai: no final with emotion scores; got %q", seen)
	}
	for k := range sum {
		sum[k] /= weight
	}
	return sum, nil
}
