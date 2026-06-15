package spclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/cenkalti/backoff/v4"
	librespot "github.com/devgianlu/go-librespot"
	connectpb "github.com/devgianlu/go-librespot/proto/spotify/connectstate"
	extmetadatapb "github.com/devgianlu/go-librespot/proto/spotify/extendedmetadata"
	"google.golang.org/protobuf/proto"
)

type Spclient struct {
	log librespot.Logger

	client *http.Client

	baseUrl     *url.URL
	clientToken string
	deviceId    string

	accessToken librespot.GetLogin5TokenFunc
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
		for name, values := range header {
			req.Header[name] = values
		}
	}

	req.Header.Set("Client-Token", c.clientToken)

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

		resp, err := c.client.Do(req.WithContext(ctx))
		if err != nil {
			return nil, err
		} else if resp.StatusCode == 401 {
			_ = resp.Body.Close()

			forceNewToken = true
			return nil, fmt.Errorf("unauthorized")
		} else if resp.StatusCode == 502 {
			_ = resp.Body.Close()

			c.log.Debugf("spclient request returned bad gateway, retrying...")
			return nil, fmt.Errorf("bad gateway")
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

			if extData.Header.StatusCode != 200 {
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
