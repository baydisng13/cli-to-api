package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// streamChunks writes the OpenAI server-sent-events contract: a first chunk
// carrying only the assistant role, then content deltas, then an empty delta
// with finish_reason, then the [DONE] sentinel.
func (s *server) streamChunks(ctx context.Context, w http.ResponseWriter, model string, chunks <-chan string, done <-chan genDone) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported", typeServerError, "")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // nginx would otherwise hold the stream
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	id := s.newID()
	created := time.Now().Unix()

	send := func(c chatChoice) {
		ev := chatResponse{ID: id, Object: "chat.completion.chunk", Created: created, Model: model, Choices: []chatChoice{c}}
		b, _ := json.Marshal(ev)
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
	}

	send(chatChoice{Index: 0, Delta: &chatMsgOut{Role: "assistant"}})

	for {
		select {
		case c, open := <-chunks:
			if !open {
				chunks = nil
			} else if c != "" {
				send(chatChoice{Index: 0, Delta: &chatMsgOut{Content: c}})
			}
		case <-ctx.Done():
			// Client hung up or the deadline fired. The adapter sees the same
			// cancellation through its emit, so there is nothing to clean up.
			return
		}
		if chunks == nil {
			break
		}
	}

	d := <-done
	if d.err != nil {
		s.log.Printf("    backend error mid-stream: %v", d.err)
		// Headers went out long ago, so the only way to tell the client is a
		// data frame it will have to notice itself.
		b, _ := json.Marshal(map[string]any{
			"error": map[string]string{"message": d.err.Error(), "type": typeBackendError},
		})
		fmt.Fprintf(w, "data: %s\n\n", b)
		flusher.Flush()
		return
	}

	// The conversation id is only known once the backend has spoken, and the
	// response headers left before the first chunk. So a newly created
	// conversation is reported on the final chunk instead; on the resume path
	// the handler has already set the X-Conversation-Id header.
	finish := "stop"
	last := chatResponse{
		ID:             id,
		Object:         "chat.completion.chunk",
		Created:        created,
		Model:          model,
		Choices:        []chatChoice{{Index: 0, Delta: &chatMsgOut{}, FinishReason: &finish}},
		ConversationID: d.res.ConversationID,
	}
	b, _ := json.Marshal(last)
	fmt.Fprintf(w, "data: %s\n\n", b)
	flusher.Flush()
	fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
