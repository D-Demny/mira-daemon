package session

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"

	librespot "github.com/devgianlu/go-librespot"
	pbdata "github.com/devgianlu/go-librespot/proto/spotify/clienttoken/data/v0"
	pbhttp "github.com/devgianlu/go-librespot/proto/spotify/clienttoken/http/v0"
	"google.golang.org/protobuf/proto"
)

func retrieveClientToken(ctx context.Context, c *http.Client, deviceId string) (string, error) {
	body, err := proto.Marshal(&pbhttp.ClientTokenRequest{
		RequestType: pbhttp.ClientTokenRequestType_REQUEST_CLIENT_DATA_REQUEST,
		Request: &pbhttp.ClientTokenRequest_ClientData{
			ClientData: &pbhttp.ClientDataRequest{
				ClientId:      librespot.ClientIdHex,
				ClientVersion: librespot.SpotifyLikeClientVersion(),
				Data: &pbhttp.ClientDataRequest_ConnectivitySdkData{
					ConnectivitySdkData: &pbdata.ConnectivitySdkData{
						DeviceId:             deviceId,
						PlatformSpecificData: librespot.GetPlatformSpecificData(),
					},
				},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("failed marshalling ClientTokenRequest: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://clienttoken.spotify.com/v1/clienttoken", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed building clienttoken request: %w", err)
	}
	req.Header.Set("Accept", "application/x-protobuf")
	req.Header.Set("User-Agent", librespot.UserAgent())

	// ctx-bound so a stalled pre-network connect is cancellable
	resp, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed requesting clienttoken: %w", err)
	}

	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("invalid status code from clienttoken: %d", resp.StatusCode)
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed reading clienttoken response: %w", err)
	}

	var protoResp pbhttp.ClientTokenResponse
	if err := proto.Unmarshal(respBody, &protoResp); err != nil {
		return "", fmt.Errorf("faield unmarshalling clienttoken response: %w", err)
	}

	switch protoResp.ResponseType {
	case pbhttp.ClientTokenResponseType_RESPONSE_GRANTED_TOKEN_RESPONSE:
		return protoResp.GetGrantedToken().Token, nil
	case pbhttp.ClientTokenResponseType_RESPONSE_CHALLENGES_RESPONSE:
		return "", fmt.Errorf("clienttoken challenge not supported")
	default:
		return "", fmt.Errorf("unknown clienttoken response type: %v", protoResp.ResponseType)
	}
}
