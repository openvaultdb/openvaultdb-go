// Package demo is the public, dependency-free OpenVaultDB client for bounded
// local application demos. It keeps control-plane credentials out of the
// database protocol client and does not import the private OVDB backend.
package demo

import (
	"context"

	"github.com/openvaultdb/openvaultdb-go/cloud"
)

type CreateSessionRequest = cloud.CreateDemoSessionRequest
type Session = cloud.DemoSession
type SessionMetadata = cloud.DemoSessionMetadata
type Option = cloud.Option

// Client calls the public Cloudflare control-plane facade.
type Client struct{ cloud *cloud.Client }

func NewClient(baseURL string, options ...Option) (*Client, error) {
	client, err := cloud.NewClient(baseURL, options...)
	if err != nil {
		return nil, err
	}
	return &Client{cloud: client}, nil
}

// CreateSession creates a one-hour-or-less Listus demo session. The caller
// supplies a device token that has the demo:write scope.
func (c *Client) CreateSession(ctx context.Context, accessToken string, request CreateSessionRequest) (Session, error) {
	return c.cloud.CreateDemoSession(ctx, accessToken, request)
}

// EndSession ends the caller's session. It is safe to retry after success.
func (c *Client) EndSession(ctx context.Context, accessToken, sessionID string) error {
	return c.cloud.EndDemoSession(ctx, accessToken, sessionID)
}

// GetSession returns only safe active-session metadata for the owner.
func (c *Client) GetSession(ctx context.Context, accessToken, spaceID string) (SessionMetadata, error) {
	return c.cloud.GetDemoSession(ctx, accessToken, spaceID)
}
