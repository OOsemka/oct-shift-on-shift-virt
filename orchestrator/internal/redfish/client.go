package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	endpoint   string
	username   string
	password   string
	httpClient *http.Client
}

func NewClient(endpoint, username, password string) *Client {
	return &Client{
		endpoint: endpoint,
		username: username,
		password: password,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		},
	}
}

type SystemInfo struct {
	ID         string `json:"Id"`
	Name       string `json:"Name"`
	PowerState string `json:"PowerState"`
	Boot       struct {
		BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
		BootSourceOverrideTarget  string `json:"BootSourceOverrideTarget"`
	} `json:"Boot"`
}

type TaskInfo struct {
	ID        string `json:"Id"`
	TaskState string `json:"TaskState"`
	Messages  []struct {
		Message string `json:"Message"`
	} `json:"Messages"`
}

func (c *Client) GetSystem(ctx context.Context, systemID string) (*SystemInfo, error) {
	url := fmt.Sprintf("%s/redfish/v1/Systems/%s", c.endpoint, systemID)
	body, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("getting system %s: %w", systemID, err)
	}
	var info SystemInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("unmarshaling system info: %w", err)
	}
	return &info, nil
}

type ResetRequest struct {
	ResetType string `json:"ResetType"`
}

func (c *Client) PowerAction(ctx context.Context, systemID, resetType string) error {
	url := fmt.Sprintf("%s/redfish/v1/Systems/%s/Actions/ComputerSystem.Reset", c.endpoint, systemID)
	payload := ResetRequest{ResetType: resetType}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling reset request: %w", err)
	}
	_, err = c.doRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("power action %s on %s: %w", resetType, systemID, err)
	}
	return nil
}

type BootOverride struct {
	Boot struct {
		BootSourceOverrideEnabled string `json:"BootSourceOverrideEnabled"`
		BootSourceOverrideTarget  string `json:"BootSourceOverrideTarget"`
	} `json:"Boot"`
}

func (c *Client) SetBootOverride(ctx context.Context, systemID, target, enabled string) error {
	url := fmt.Sprintf("%s/redfish/v1/Systems/%s", c.endpoint, systemID)
	override := BootOverride{}
	override.Boot.BootSourceOverrideEnabled = enabled
	override.Boot.BootSourceOverrideTarget = target
	body, err := json.Marshal(override)
	if err != nil {
		return fmt.Errorf("marshaling boot override: %w", err)
	}
	_, err = c.doRequest(ctx, http.MethodPatch, url, body)
	if err != nil {
		return fmt.Errorf("setting boot override on %s: %w", systemID, err)
	}
	return nil
}

type InsertMediaRequest struct {
	Image string `json:"Image"`
}

func (c *Client) InsertMedia(ctx context.Context, systemID, isoURL string) (string, error) {
	url := fmt.Sprintf("%s/redfish/v1/Systems/%s/VirtualMedia/Cd/Actions/VirtualMedia.InsertMedia",
		c.endpoint, systemID)
	payload := InsertMediaRequest{Image: isoURL}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling insert media request: %w", err)
	}
	respBody, err := c.doRequest(ctx, http.MethodPost, url, body)
	if err != nil {
		return "", fmt.Errorf("inserting media on %s: %w", systemID, err)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(respBody, &resp); err == nil {
		if taskID, ok := resp["Id"].(string); ok {
			return taskID, nil
		}
	}
	return "", nil
}

func (c *Client) EjectMedia(ctx context.Context, systemID string) error {
	url := fmt.Sprintf("%s/redfish/v1/Systems/%s/VirtualMedia/Cd/Actions/VirtualMedia.EjectMedia",
		c.endpoint, systemID)
	_, err := c.doRequest(ctx, http.MethodPost, url, []byte("{}"))
	if err != nil {
		return fmt.Errorf("ejecting media on %s: %w", systemID, err)
	}
	return nil
}

func (c *Client) GetTaskStatus(ctx context.Context, taskID string) (*TaskInfo, error) {
	url := fmt.Sprintf("%s/redfish/v1/TaskService/Tasks/%s", c.endpoint, taskID)
	body, err := c.doRequest(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("getting task %s: %w", taskID, err)
	}
	var info TaskInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("unmarshaling task info: %w", err)
	}
	return &info, nil
}

func (c *Client) doRequest(ctx context.Context, method, url string, body []byte) ([]byte, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return respBody, nil
}
