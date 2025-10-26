package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type ChatRequest struct {
	ChatID       string `json:"chat_id"`
	SystemPrompt string `json:"system_prompt"`
	UserPrompt   string `json:"user_prompt"`
}

type BatchedRequest struct {
	Queries []ChatRequest `json:"queries"`
}

type HostResponse struct {
	ChatID   string `json:"chat_id"`
	Response string `json:"response,omitempty"`
	Error    string `json:"error,omitempty"`
}

// semaphore to limit concurrent requests to hosting service
var hostingSem chan struct{}

func callHosting(ctx context.Context, hostURL string, req ChatRequest) (HostResponse, error) {
	var hr HostResponse
	body, _ := json.Marshal(req)
	request, _ := http.NewRequestWithContext(ctx, "POST", hostURL+"/chat", bytes.NewBuffer(body))
	request.Header.Set("Content-Type", "application/json")
	// Acquire semaphore if configured
	if hostingSem != nil {
		hostingSem <- struct{}{}
		defer func() { <-hostingSem }()
	}

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(request)
	if err != nil {
		return hr, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return hr, &httpError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	err = json.Unmarshal(data, &hr)
	return hr, err
}

// callHostingBatch attempts to call a hosting endpoint that accepts a batch of queries in one request.
// It posts to hostURL + "/generate_batch" with the same BatchedRequest JSON format and expects
// {"responses": [...]} in return. If the hosting endpoint is not available, callers can fall back
// to calling callHosting per-item.
func callHostingBatch(ctx context.Context, hostURL string, queries []ChatRequest) ([]interface{}, error) {
	type batchReq struct{
		Queries []ChatRequest `json:"queries"`
	}
	type batchResp struct{
		Responses []json.RawMessage `json:"responses"`
	}
	var br batchReq
	br.Queries = queries
	body, _ := json.Marshal(br)
	request, _ := http.NewRequestWithContext(ctx, "POST", hostURL+"/generate_batch", bytes.NewBuffer(body))
	request.Header.Set("Content-Type", "application/json")

	if hostingSem != nil {
		hostingSem <- struct{}{}
		defer func() { <-hostingSem }()
	}

	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, &httpError{StatusCode: resp.StatusCode, Body: string(data)}
	}
	var brp batchResp
	if err := json.Unmarshal(data, &brp); err != nil {
		return nil, err
	}
	
	// Convert raw messages to proper interface{} objects
	var responses []interface{}
	for _, rawResp := range brp.Responses {
		var respObj map[string]interface{}
		if err := json.Unmarshal(rawResp, &respObj); err != nil {
			return nil, err
		}
		responses = append(responses, respObj)
	}
	return responses, nil
}

type httpError struct {
	StatusCode int
	Body       string
}

func (e *httpError) Error() string { return e.Body }

func main() {
	r := gin.Default()
	hosting := "http://127.0.0.1:8001"

	// configure max concurrent requests to hosting via env var HOSTING_MAX_CONCURRENCY (default 8)
	maxConc := 8
	if v, ok := os.LookupEnv("HOSTING_MAX_CONCURRENCY"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxConc = n
		}
	}
	hostingSem = make(chan struct{}, maxConc)

	r.POST("/chat", func(c *gin.Context) {
		var req ChatRequest
		if err := c.BindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		// basic validation
		if req.ChatID == "" {
			c.JSON(400, gin.H{"error": "chat_id is required and cannot be empty"})
			return
		}
		if req.UserPrompt == "" {
			c.JSON(400, gin.H{"error": "user_prompt is required and cannot be empty"})
			return
		}
		// Note: system_prompt is optional, but if provided should not be empty
		// We'll let the hosting service handle more detailed validation
		ctx := c.Request.Context()
		resp, err := callHosting(ctx, hosting, req)
		if err != nil {
			// Clean up error messages for users
			if httpErr, ok := err.(*httpError); ok {
				c.JSON(httpErr.StatusCode, gin.H{"error": "Model service error", "details": httpErr.Body})
			} else {
				c.JSON(500, gin.H{"error": "Model service temporarily unavailable"})
			}
			return
		}
		c.JSON(200, resp)
	})

	r.POST("/chat/batched", func(c *gin.Context) {
		var br BatchedRequest
		if err := c.BindJSON(&br); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		// Validate queries array is not empty
		if len(br.Queries) == 0 {
			c.JSON(400, gin.H{"error": "queries array required and must be non-empty"})
			return
		}
		// For partial processing, we'll validate during processing
		// rather than failing the entire batch upfront	
		ctx := c.Request.Context()
		log.Printf(" Processing batch request with %d queries", len(br.Queries))
		// Try batch endpoint first for efficiency (DYNAMIC BATCHING)
		if respBatch, err := callHostingBatch(ctx, hosting, br.Queries); err != nil {
			log.Printf("  Dynamic batch endpoint failed: %v, falling back to concurrent processing", err)
		} else {
			log.Printf(" SUCCESS: Using dynamic batching endpoint - processed %d queries in single model call", len(br.Queries))
			// Check if there are any errors in the batch response
			hasErrors := false
			for _, resp := range respBatch {
				if respMap, ok := resp.(map[string]interface{}); ok {
					if _, hasError := respMap["error"]; hasError {
						hasErrors = true
						break
					}
				}
			}
			responseObj := gin.H{"responses": respBatch}
			if hasErrors {
				responseObj["partial_success"] = true
				responseObj["message"] = "Some queries failed but others succeeded"
			}
			c.JSON(200, responseObj)
			return
		}
		log.Println(" Falling back to concurrent per-item processing")

		// Fallback: process concurrently per-item using goroutines with partial failure support
		n := len(br.Queries)
		results := make([]HostResponse, n)
		errs := make([]string, n)
		var wg sync.WaitGroup
		wg.Add(n)
		for i, q := range br.Queries {
			go func(i int, q ChatRequest) {
				defer wg.Done()
				// Validate individual query
				if q.ChatID == "" {
					errs[i] = "chat_id is required and cannot be empty"
					return
				}
				if q.UserPrompt == "" {
					errs[i] = "user_prompt is required and cannot be empty"
					return
				}
				
				resp, err := callHosting(ctx, hosting, q)
				if err != nil {
					// Clean up error messages for users
					if httpErr, ok := err.(*httpError); ok {
						errs[i] = fmt.Sprintf("Model service error: %s", httpErr.Body)
					} else {
						errs[i] = "Model service temporarily unavailable"
					}
					return
				}
				results[i] = resp
			}(i, q)
		}
		wg.Wait()
		
		// Build response with partial results
		var responses []interface{}
		var hasErrors bool
		
		for i := 0; i < n; i++ {
			if errs[i] != "" {
				// Include error for this specific query
				responses = append(responses, map[string]interface{}{
					"chat_id": br.Queries[i].ChatID,
					"error":   errs[i],
				})
				hasErrors = true
			} else {
				// Include successful response
				responses = append(responses, results[i])
			}
		}
		// Return 200 with partial results (batch APIs)
		responseObj := gin.H{"responses": responses}
		if hasErrors {
			responseObj["partial_success"] = true
			responseObj["message"] = "Some queries failed but others succeeded"
		}
		c.JSON(200, responseObj)
	})

	log.Println("✅ API server running at http://127.0.0.1:8000")
	r.Run(":8000")
}
