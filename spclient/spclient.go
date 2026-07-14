package spclient

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	"google.golang.org/protobuf/proto"
)

func isRetryableHTTPStatus(status int) bool {
	switch status {
	case http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}

type Spclient struct {
	log librespot.Logger

	client *http.Client

	baseUrl     *url.URL
	clientToken string
	deviceId    string

	accessToken librespot.GetLogin5TokenFunc

	// WebPlayer client-token for the Pathfinder gateway, minted + cached
	// separately from the native client-token above.
	webClientToken    string
	webClientTokenExp time.Time
	webClientTokenMu  sync.Mutex
}

func NewSpclient(ctx context.Context, log librespot.Logger, client *http.Client, addr librespot.GetAddressFunc, accessToken librespot.GetLogin5TokenFunc, deviceId, clientToken string) (*Spclient, error) {
	baseUrl, err := url.Parse(fmt.Sprintf("https://%s/", addr(ctx)))
	if err != nil {
		return nil, fmt.Errorf("invalid spclient base url: %w", err)
	}

	return &Spclient{
		log:         log,
		client:      client,
		baseUrl:     baseUrl,
		clientToken: clientToken,
		deviceId:    deviceId,
		accessToken: accessToken,
	}, nil
}

func (c *Spclient) innerRequest(ctx context.Context, method string, reqUrl *url.URL, query url.Values, header http.Header, body []byte) (*http.Response, error) {
	if query != nil {
		reqUrl.RawQuery = query.Encode()
	}

	req := &http.Request{
		URL:    reqUrl,
		Method: method,
		Header: http.Header{},
	}

	if header != nil {
		maps.Copy(req.Header, header)
	}

	if len(c.clientToken) > 0 {
		req.Header.Set("Client-Token", c.clientToken)
	}

	if body != nil {
		// default protobuf, caller can pin json (used by connect-state commands)
		if _, ok := req.Header["Content-Type"]; !ok {
			req.Header.Set("Content-Type", "application/x-protobuf")
		}

		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
		req.Body, _ = req.GetBody()
	}

	var forceNewToken bool
	resp, err := backoff.RetryWithData(func() (*http.Response, error) {
		// restore the request body on every attempt
		if req.GetBody != nil {
			req.Body, _ = req.GetBody()
		}

		accessToken, err := c.accessToken(ctx, forceNewToken)
		if err != nil {
			// Fail with a permanent error if we can't get a new token. The caller should have already retried, there's
			// nothing we can do.
			return nil, backoff.Permanent(fmt.Errorf("failed obtaining spclient access token: %w", err))
		}

		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", accessToken))

		// The request body is consumed and normally closed by Client.Do.
		// Recreate it for every attempt.
		if req.GetBody != nil {
			req.Body, err = req.GetBody()
			if err != nil {
				return nil, backoff.Permanent(
					fmt.Errorf("failed recreating request body: %w", err),
				)
			}
		}

		resp, err := c.client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		}

		if resp.StatusCode == http.StatusUnauthorized {
			_ = resp.Body.Close()

			forceNewToken = true
			return nil, fmt.Errorf("unauthorized")
		}

		if isRetryableHTTPStatus(resp.StatusCode) {
			status := resp.StatusCode
			_ = resp.Body.Close()
			c.log.Debugf(
				"spclient request returned transient status %d, retrying...",
				status,
			)
			return nil, fmt.Errorf(
				"spclient request returned transient status %d",
				status,
			)
		}

		return resp, nil
	}, backoff.WithContext(backoff.NewExponentialBackOff(), ctx))
	if err != nil {
		return nil, fmt.Errorf("spclient request failed: %w", err)
	}

	return resp, nil
}

func (c *Spclient) WebApiRequest(ctx context.Context, method string, path string, query url.Values, header http.Header, body []byte) (*http.Response, error) {
	reqPath, err := url.Parse("https://api.spotify.com/")
	if err != nil {
		panic("invalid api base url")
	}
	reqURL := reqPath.JoinPath(path)
	return c.innerRequest(ctx, method, reqURL, query, header, body)
}

func (c *Spclient) Request(ctx context.Context, method string, path string, query url.Values, header http.Header, body []byte) (*http.Response, error) {
	reqUrl := c.baseUrl.JoinPath(path)
	return c.innerRequest(ctx, method, reqUrl, query, header, body)
}

const (
	webPlayerClientId    = "d8a5ed958d274c2e8ee717e6a4b0971d"
	spotifyWebAppVersion = "1.2.80.313.gd1726b65"
	spotifyWebUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36"
)

func (c *Spclient) PartnerApiRequest(ctx context.Context, body []byte) (*http.Response, error) {
	return c.PartnerApiRequestEx(ctx, body, false)
}

func (c *Spclient) PartnerApiRequestEx(ctx context.Context, body []byte, force bool) (*http.Response, error) {
	if force {
		c.InvalidateWebPlayerClientToken()
	}
	accessToken, err := c.accessToken(ctx, force)
	if err != nil {
		return nil, fmt.Errorf("failed obtaining access token: %w", err)
	}
	clientToken, err := c.webPlayerClientToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed obtaining web client token: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api-partner.spotify.com/pathfinder/v2/query", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed building pathfinder request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("App-Platform", "WebPlayer")
	req.Header.Set("Spotify-App-Version", spotifyWebAppVersion)
	req.Header.Set("User-Agent", spotifyWebUserAgent)
	req.Header.Set("Origin", "https://open.spotify.com")
	req.Header.Set("Client-Token", clientToken)
	return c.client.Do(req)
}

func (c *Spclient) InvalidateWebPlayerClientToken() {
	c.webClientTokenMu.Lock()
	c.webClientToken = ""
	c.webClientTokenExp = time.Time{}
	c.webClientTokenMu.Unlock()
}

func (c *Spclient) webPlayerClientToken(ctx context.Context) (string, error) {
	c.webClientTokenMu.Lock()
	defer c.webClientTokenMu.Unlock()

	if c.webClientToken != "" && time.Now().Before(c.webClientTokenExp) {
		return c.webClientToken, nil
	}

	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fmt.Errorf("failed generating device id: %w", err)
	}
	reqBody, _ := json.Marshal(map[string]any{
		"client_data": map[string]any{
			"client_version": spotifyWebAppVersion,
			"client_id":      webPlayerClientId,
			"js_sdk_data": map[string]any{
				"device_brand": "Apple",
				"device_model": "unknown",
				"os":           "macos",
				"os_version":   "10.15.7",
				"device_id":    hex.EncodeToString(idBytes),
				"device_type":  "computer",
			},
		},
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://clienttoken.spotify.com/v1/clienttoken", bytes.NewReader(reqBody))
	if err != nil {
		return "", fmt.Errorf("failed building clienttoken request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed requesting web client token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("web clienttoken returned status %d", resp.StatusCode)
	}

	var data struct {
		GrantedToken struct {
			Token               string `json:"token"`
			ExpiresAfterSeconds int    `json:"expires_after_seconds"`
		} `json:"granted_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return "", fmt.Errorf("failed decoding web clienttoken: %w", err)
	}
	if data.GrantedToken.Token == "" {
		return "", fmt.Errorf("web clienttoken response had no token")
	}

	ttl := data.GrantedToken.ExpiresAfterSeconds
	if ttl <= 0 {
		ttl = 3600
	}
	c.webClientToken = data.GrantedToken.Token
	c.webClientTokenExp = time.Now().Add(time.Duration(ttl-60) * time.Second)
	return c.webClientToken, nil
}

type putStateError struct {
	ErrorType string `json:"error_type"`
	Message   string `json:"message"`
}

func (c *Spclient) PutConnectStateInactive(ctx context.Context, spotConnId string, notify bool) error {
	resp, err := c.Request(
		ctx,
		"PUT",
		fmt.Sprintf("/connect-state/v1/devices/%s/inactive", c.deviceId),
		url.Values{"notify": []string{strconv.FormatBool(notify)}},
		http.Header{
			"X-Spotify-Connection-Id": []string{spotConnId},
		},
		nil,
	)
	if err != nil {
		return err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 204 {
		return fmt.Errorf("put state inactive request failed with status %d", resp.StatusCode)
	} else {
		c.log.Debug("put connect state inactive")
		return nil
	}
}

// PutConnectState registers/updates our connect-state
func (c *Spclient) PutConnectState(ctx context.Context, spotConnId string, reqProto *connectpb.PutStateRequest) (*connectpb.Cluster, error) {
	reqBody, err := proto.Marshal(reqProto)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling PutStateRequest: %w", err)
	}
	cluster, err := backoff.RetryWithData(func() (*connectpb.Cluster, error) {
		resp, err := c.Request(
			ctx,
			"PUT",
			fmt.Sprintf("/connect-state/v1/devices/%s", c.deviceId),
			nil,
			http.Header{
				"X-Spotify-Connection-Id": []string{spotConnId},
				"Content-Type":            []string{"application/x-protobuf"},
			},
			reqBody,
		)
		if err != nil {
			return nil, err
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != 200 {
			var putError putStateError
			if err := json.NewDecoder(resp.Body).Decode(&putError); err != nil {
				c.log.Debugf("failed reading error response %s", err)
				return nil, fmt.Errorf("failed reading error response: %w", err)
			}
			c.log.Debugf("put state request failed with status %d: %s", resp.StatusCode, putError.Message)
			return nil, fmt.Errorf("put state request failed with status %d: %s", resp.StatusCode, putError.Message)
		}

		c.log.Debugf("put connect state because %s", reqProto.PutStateReason)
		// the 200 body is the current Cluster
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			c.log.Debugf("put connect state: failed reading cluster response: %v", err)
			return nil, nil
		}
		var cluster connectpb.Cluster
		if err := proto.Unmarshal(body, &cluster); err != nil {
			c.log.Debugf("put connect state: response was not a cluster (%d bytes): %v", len(body), err)
			return nil, nil
		}
		return &cluster, nil
	}, backoff.WithContext(backoff.WithMaxRetries(backoff.NewConstantBackOff(1*time.Second), 2), ctx))
	if err != nil {
		return nil, err
	}
	return cluster, nil
}

// SendPlayerCommand sends a Spotify Connect remote-control command,
// used by observer mode to drive playback without going through the Web API
func (c *Spclient) SendPlayerCommand(ctx context.Context, fromDeviceId, toDeviceId, spotConnId string, body []byte) error {
	if fromDeviceId == "" || toDeviceId == "" {
		return fmt.Errorf("send player command: missing device id (from=%q to=%q)", fromDeviceId, toDeviceId)
	}
	if spotConnId == "" {
		return fmt.Errorf("send player command: missing spotify-connection-id")
	}
	resp, err := c.Request(
		ctx,
		"POST",
		fmt.Sprintf("/connect-state/v1/player/command/from/%s/to/%s", fromDeviceId, toDeviceId),
		nil,
		http.Header{
			"X-Spotify-Connection-Id": []string{spotConnId},
			"Content-Type":            []string{"application/json"},
		},
		body,
	)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("send player command: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// transfers the clusters current playback session to another device
func (c *Spclient) TransferConnect(ctx context.Context, fromDeviceId, toDeviceId, spotConnId string, body []byte) error {
	if fromDeviceId == "" || toDeviceId == "" {
		return fmt.Errorf("transfer connect: missing device id (from=%q to=%q)", fromDeviceId, toDeviceId)
	}
	if spotConnId == "" {
		return fmt.Errorf("transfer connect: missing spotify-connection-id")
	}
	resp, err := c.Request(
		ctx,
		"POST",
		fmt.Sprintf("/connect-state/v1/connect/transfer/from/%s/to/%s", fromDeviceId, toDeviceId),
		nil,
		http.Header{
			"X-Spotify-Connection-Id": []string{spotConnId},
			"Content-Type":            []string{"application/json"},
		},
		body,
	)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("transfer connect: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// sets the volume of a remote device
func (c *Spclient) SetConnectVolume(ctx context.Context, fromDeviceId, toDeviceId, spotConnId string, body []byte) error {
	if fromDeviceId == "" || toDeviceId == "" {
		return fmt.Errorf("set connect volume: missing device id (from=%q to=%q)", fromDeviceId, toDeviceId)
	}
	if spotConnId == "" {
		return fmt.Errorf("set connect volume: missing spotify-connection-id")
	}
	resp, err := c.Request(
		ctx,
		"PUT",
		fmt.Sprintf("/connect-state/v1/connect/volume/from/%s/to/%s", fromDeviceId, toDeviceId),
		nil,
		http.Header{
			"X-Spotify-Connection-Id": []string{spotConnId},
			"Content-Type":            []string{"application/json"},
		},
		body,
	)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("set connect volume: status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

func (c *Spclient) ExtendedMetadata(ctx context.Context, req *extmetadatapb.BatchedEntityRequest) (*extmetadatapb.BatchedExtensionResponse, error) {
	reqBody, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed marshalling BatchedEntityRequest: %w", err)
	}

	resp, err := c.Request(ctx, "POST", "/extended-metadata/v0/extended-metadata", nil, nil, reqBody)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("invalid status code from extended metadata: %d", resp.StatusCode)
	}

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed reading response body: %w", err)
	}

	var protoResp extmetadatapb.BatchedExtensionResponse
	if err := proto.Unmarshal(respBytes, &protoResp); err != nil {
		return nil, fmt.Errorf("failed unmarshalling BatchedExtensionResponse: %w", err)
	}

	return &protoResp, nil
}

func (c *Spclient) ExtendedMetadataSimple(ctx context.Context, id librespot.SpotifyId, ext extmetadatapb.ExtensionKind, data proto.Message) error {
	resp, err := c.ExtendedMetadata(ctx, &extmetadatapb.BatchedEntityRequest{
		EntityRequest: []*extmetadatapb.EntityRequest{{
			EntityUri: id.Uri(),
			Query: []*extmetadatapb.ExtensionQuery{{
				ExtensionKind: ext,
			}},
		}},
	})
	if err != nil {
		return err
	}

	for _, item := range resp.ExtendedMetadata {
		if item.ExtensionKind != ext {
			continue
		}

		for _, extData := range item.ExtensionData {
			if extData.EntityUri != id.Uri() {
				continue
			}

			if extData.Header != nil && extData.Header.StatusCode != 200 {
				return fmt.Errorf("extended metadata request returned status %d", extData.Header.StatusCode)
			}

			if err := extData.ExtensionData.UnmarshalTo(data); err != nil {
				return fmt.Errorf("failed unmarshalling extended metadata data: %w", err)
			}

			return nil
		}
	}

	return fmt.Errorf("extended metadata with kind %s not found", ext)
}

func (c *Spclient) GetAccessToken(ctx context.Context, force bool) (string, error) {
	return c.accessToken(ctx, force)
}
