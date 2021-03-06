package api

import "fmt"

// SSH is used to return a client to invoke operations on SSH backend.
type SSH struct {
	c          *Client
	MountPoint string
}

// Returns the client for logical-backend API calls.
func (c *Client) SSH() *SSH {
	return c.SSHWithMountPoint(SSHAgentDefaultMountPoint)
}

// Returns the client with specific SSH mount point.
func (c *Client) SSHWithMountPoint(mountPoint string) *SSH {
	return &SSH{
		c:          c,
		MountPoint: mountPoint,
	}
}

// Invokes the SSH backend API to create a credential to establish an SSH session.
func (c *SSH) Credential(role string, data map[string]interface{}) (*Secret, error) {
	r := c.c.NewRequest("PUT", fmt.Sprintf("/v1/%s/creds/%s", c.MountPoint, role))
	if err := r.SetJSONBody(data); err != nil {
		return nil, err
	}

	resp, err := c.c.RawRequest(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return ParseSecret(resp.Body)
}
