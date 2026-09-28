// Package docker talks to the Docker Engine API over /var/run/docker.sock with plain
// net/http, and runs the docker CLI for what the API doesn't do (compose, registry digests).
package docker

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const Socket = "/var/run/docker.sock"

type Client struct{ http *http.Client }

func New() *Client {
	return &Client{http: &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", Socket)
		}},
	}}
}

// IDs and names in URL paths: container/image IDs, names, and image references.
var idRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.:/@-]*$`)

func ValidID(id string) bool { return len(id) <= 256 && idRe.MatchString(id) }

type apiError struct {
	Message string `json:"message"`
}

func (c *Client) do(method, path string, q url.Values, out any) error {
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("docker isn't reachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		var e apiError
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Message == "" {
			e.Message = resp.Status
		}
		return errors.New(e.Message)
	}
	if out == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type Port struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	ImageID string            `json:"ImageID"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Created int64             `json:"Created"`
	Ports   []Port            `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
}

func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:12]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

func (c *Client) Containers() ([]Container, error) {
	var out []Container
	err := c.do("GET", "/containers/json", url.Values{"all": {"1"}}, &out)
	return out, err
}

func (c *Client) ContainerAction(id, action string) error {
	switch action {
	case "start", "stop", "restart", "pause", "unpause", "kill":
		return c.do("POST", "/containers/"+id+"/"+action, nil, nil)
	case "remove":
		return c.do("DELETE", "/containers/"+id, url.Values{"force": {"1"}}, nil)
	}
	return errors.New("unknown action")
}

// Logs returns the last lines of a container's output. Without a TTY the stream is
// multiplexed: each frame has an 8-byte header whose last 4 bytes are the length.
func (c *Client) Logs(id string, tail int) (string, error) {
	var info struct {
		Config struct{ Tty bool } `json:"Config"`
	}
	if err := c.do("GET", "/containers/"+id+"/json", nil, &info); err != nil {
		return "", err
	}
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "tail": {fmt.Sprint(tail)}}
	req, _ := http.NewRequest("GET", "http://docker/containers/"+id+"/logs?"+q.Encode(), nil)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if info.Config.Tty {
		return string(body), nil
	}
	return Demux(body), nil
}

func Demux(b []byte) string {
	var sb strings.Builder
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b[4:8]))
		b = b[8:]
		if n > len(b) {
			n = len(b)
		}
		sb.Write(b[:n])
		b = b[n:]
	}
	return sb.String()
}

type Stats struct {
	CPUPercent float64 `json:"cpuPercent"`
	MemUsed    uint64  `json:"memUsed"`
	MemLimit   uint64  `json:"memLimit"`
	NetRx      uint64  `json:"netRx"`
	NetTx      uint64  `json:"netTx"`
}

func (c *Client) ContainerStats(id string) (Stats, error) {
	var raw struct {
		CPU struct {
			Usage struct {
				Total uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			System uint64 `json:"system_cpu_usage"`
			Online int    `json:"online_cpus"`
		} `json:"cpu_stats"`
		Pre struct {
			Usage struct {
				Total uint64 `json:"total_usage"`
			} `json:"cpu_usage"`
			System uint64 `json:"system_cpu_usage"`
		} `json:"precpu_stats"`
		Mem struct {
			Usage uint64            `json:"usage"`
			Limit uint64            `json:"limit"`
			Stats map[string]uint64 `json:"stats"`
		} `json:"memory_stats"`
		Networks map[string]struct {
			Rx uint64 `json:"rx_bytes"`
			Tx uint64 `json:"tx_bytes"`
		} `json:"networks"`
	}
	if err := c.do("GET", "/containers/"+id+"/stats", url.Values{"stream": {"0"}}, &raw); err != nil {
		return Stats{}, err
	}
	var s Stats
	cd := float64(raw.CPU.Usage.Total) - float64(raw.Pre.Usage.Total)
	sd := float64(raw.CPU.System) - float64(raw.Pre.System)
	if sd > 0 && cd > 0 {
		s.CPUPercent = cd / sd * float64(max(raw.CPU.Online, 1)) * 100
	}
	s.MemUsed = raw.Mem.Usage
	if inactive := raw.Mem.Stats["inactive_file"]; inactive < s.MemUsed {
		s.MemUsed -= inactive
	}
	s.MemLimit = raw.Mem.Limit
	for _, n := range raw.Networks {
		s.NetRx += n.Rx
		s.NetTx += n.Tx
	}
	return s, nil
}

type Image struct {
	ID       string   `json:"Id"`
	RepoTags []string `json:"RepoTags"`
	Size     int64    `json:"Size"`
	Created  int64    `json:"Created"`
	InUse    int      `json:"Containers"`
}

func (c *Client) Images() ([]Image, error) {
	var out []Image
	err := c.do("GET", "/images/json", nil, &out)
	return out, err
}

func (c *Client) RemoveImage(id string) error {
	return c.do("DELETE", "/images/"+id, nil, nil)
}

type Volume struct {
	Name       string `json:"Name"`
	Driver     string `json:"Driver"`
	Mountpoint string `json:"Mountpoint"`
	CreatedAt  string `json:"CreatedAt"`
}

func (c *Client) Volumes() ([]Volume, error) {
	var out struct{ Volumes []Volume }
	err := c.do("GET", "/volumes", nil, &out)
	return out.Volumes, err
}

type Network struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Driver string `json:"Driver"`
	Scope  string `json:"Scope"`
}

func (c *Client) Networks() ([]Network, error) {
	var out []Network
	err := c.do("GET", "/networks", nil, &out)
	return out, err
}

// Prune removes unused containers, images (all unused, not just dangling), volumes or networks.
func (c *Client) Prune(what string) (uint64, error) {
	var q url.Values
	switch what {
	case "containers", "networks":
	case "images":
		q = url.Values{"filters": {`{"dangling":["false"]}`}}
	case "volumes":
		// Since API 1.42 only anonymous volumes are pruned unless asked for all.
		q = url.Values{"filters": {`{"all":["true"]}`}}
	default:
		return 0, errors.New("unknown prune target")
	}
	var out struct {
		SpaceReclaimed uint64 `json:"SpaceReclaimed"`
	}
	err := c.do("POST", "/"+what+"/prune", q, &out)
	return out.SpaceReclaimed, err
}

type Info struct {
	Containers        int    `json:"Containers"`
	ContainersRunning int    `json:"ContainersRunning"`
	ContainersStopped int    `json:"ContainersStopped"`
	Images            int    `json:"Images"`
	ServerVersion     string `json:"ServerVersion"`
}

func (c *Client) Info() (Info, error) {
	var out Info
	err := c.do("GET", "/info", nil, &out)
	return out, err
}

func (c *Client) ImageDigests(ref string) ([]string, error) {
	var out struct {
		RepoDigests []string `json:"RepoDigests"`
	}
	err := c.do("GET", "/images/"+ref+"/json", nil, &out)
	return out.RepoDigests, err
}
