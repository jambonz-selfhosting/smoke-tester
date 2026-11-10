package studio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// KBToolName is the tool a pipeline agent calls to search its knowledge bases.
const KBToolName = "search_knowledge_base"

// CreateEmbeddingCredential stores an embedding API key in the workspace and returns its id.
func (c *Client) CreateEmbeddingCredential(ctx context.Context, name, vendor, apiKey string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.Do(ctx, http.MethodPost, "/embedding-credentials", map[string]any{"name": name, "vendor": vendor, "api_key": apiKey}, &out)
	return out.ID, err
}

func (c *Client) DeleteEmbeddingCredential(ctx context.Context, id string) error {
	return c.Do(ctx, http.MethodDelete, "/embedding-credentials/"+id, nil, nil)
}

// CreateKnowledgeBase makes an English knowledge base embedded with model and returns its id.
func (c *Client) CreateKnowledgeBase(ctx context.Context, name, credentialID, model string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := c.Do(ctx, http.MethodPost, "/knowledge-bases", map[string]any{
		"name": name, "lang": "en", "embed_credential_id": credentialID, "embed_model": model,
	}, &out)
	return out.ID, err
}

// DeleteKnowledgeBase deletes a knowledge base and waits until it is gone; Studio removes it in the background.
func (c *Client) DeleteKnowledgeBase(ctx context.Context, id string) error {
	if err := c.Do(ctx, http.MethodDelete, "/knowledge-bases/"+id, nil, nil); err != nil {
		return err
	}
	for {
		err := c.Do(ctx, http.MethodGet, "/knowledge-bases/"+id, nil, nil)
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("knowledge base %s not gone: %w", id, ctx.Err())
		case <-time.After(time.Second):
		}
	}
}

// UploadDocument uploads text as a file to a knowledge base and returns the document id.
func (c *Client) UploadDocument(ctx context.Context, kbID, filename, text string) (string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "", err
	}
	_, _ = io.WriteString(fw, text)
	_ = mw.Close()
	path := fmt.Sprintf("%s/accounts/%s/knowledge-bases/%s/documents", c.apiBase, c.AccountID, kbID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, path, &buf)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cookie", sessionCookie+"="+c.sid)
	req.Header.Set("Origin", c.origin)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return "", &APIError{Method: http.MethodPost, Path: strings.TrimPrefix(path, c.apiBase), Status: res.StatusCode, Body: string(data)}
	}
	var out struct {
		ID       string `json:"id"`
		Document *struct {
			ID string `json:"id"`
		} `json:"document"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.Document != nil {
		return out.Document.ID, nil
	}
	return out.ID, nil
}

// WaitIndexed polls a document until it is indexed; a failed document returns its error code.
func (c *Client) WaitIndexed(ctx context.Context, kbID, docID string) error {
	for {
		var docs []struct {
			ID        string `json:"id"`
			Status    string `json:"status"`
			ErrorCode string `json:"error_code"`
		}
		if err := c.Do(ctx, http.MethodGet, "/knowledge-bases/"+kbID+"/documents", nil, &docs); err != nil {
			return err
		}
		status := "missing"
		for _, d := range docs {
			if d.ID == docID {
				status = d.Status
				if d.Status == "failed" {
					return fmt.Errorf("document %s failed to index: %s", docID, d.ErrorCode)
				}
			}
		}
		if status == "indexed" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("document %s still %s: %w", docID, status, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}
