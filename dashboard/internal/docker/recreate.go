package docker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

func (c *Client) doBody(method, path string, q url.Values, body, out any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u := "http://docker" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequest(method, u, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
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

type obj = map[string]any

// CloneSpec turns `docker inspect` output into a create request for the same container.
// Values that only came from the old image (its Env, Cmd, labels, …) are left out, so the
// new image's defaults apply; everything the container was started with is kept. Volume
// mounts, anonymous ones included, are passed by name so the new container gets the same
// data.
func CloneSpec(inspect, imageConfig obj) obj {
	cfg, _ := inspect["Config"].(obj)
	if cfg == nil {
		cfg = obj{}
	}
	cfg = copyMap(cfg)
	id, _ := inspect["Id"].(string)
	if h, _ := cfg["Hostname"].(string); len(id) >= 12 && h == id[:12] {
		delete(cfg, "Hostname")
	}
	for _, k := range []string{"Cmd", "Entrypoint", "WorkingDir", "User", "ExposedPorts", "Volumes",
		"Healthcheck", "StopSignal", "OnBuild", "Shell"} {
		if v, ok := imageConfig[k]; ok && reflect.DeepEqual(cfg[k], v) {
			delete(cfg, k)
		}
	}
	if env, ok := cfg["Env"].([]any); ok {
		imgEnv, _ := imageConfig["Env"].([]any)
		var keep []any
		for _, e := range env {
			if !slices.ContainsFunc(imgEnv, func(x any) bool { return x == e }) {
				keep = append(keep, e)
			}
		}
		cfg["Env"] = keep
	}
	if labels, ok := cfg["Labels"].(obj); ok {
		imgLabels, _ := imageConfig["Labels"].(obj)
		for k, v := range imgLabels {
			if labels[k] == v {
				delete(labels, k)
			}
		}
	}

	host, _ := inspect["HostConfig"].(obj)
	host = copyMap(host)
	binds, _ := host["Binds"].([]any)
	mounts, _ := host["Mounts"].([]any)
	has := func(dest string) bool {
		for _, b := range binds {
			parts := strings.Split(fmt.Sprint(b), ":")
			if len(parts) >= 2 && parts[1] == dest {
				return true
			}
		}
		for _, m := range mounts {
			if mm, ok := m.(obj); ok && mm["Target"] == dest {
				return true
			}
		}
		return false
	}
	ms, _ := inspect["Mounts"].([]any)
	for _, m := range ms {
		mm, ok := m.(obj)
		if !ok || mm["Type"] != "volume" {
			continue
		}
		name, _ := mm["Name"].(string)
		dest, _ := mm["Destination"].(string)
		if name == "" || dest == "" || has(dest) {
			continue
		}
		b := name + ":" + dest
		if rw, ok := mm["RW"].(bool); ok && !rw {
			b += ":ro"
		}
		binds = append(binds, b)
	}
	if len(binds) > 0 {
		host["Binds"] = binds
	}

	endpoints := obj{}
	if ns, ok := inspect["NetworkSettings"].(obj); ok {
		nets, _ := ns["Networks"].(obj)
		for name, v := range nets {
			ep, ok := v.(obj)
			if !ok {
				continue
			}
			out := obj{}
			for _, k := range []string{"IPAMConfig", "Links", "DriverOpts"} {
				if ep[k] != nil {
					out[k] = ep[k]
				}
			}
			if aliases, ok := ep["Aliases"].([]any); ok {
				var keep []any
				for _, a := range aliases {
					if s, _ := a.(string); len(id) >= 12 && s == id[:12] {
						continue
					}
					keep = append(keep, a)
				}
				if len(keep) > 0 {
					out["Aliases"] = keep
				}
			}
			endpoints[name] = out
		}
	}

	spec := cfg
	spec["HostConfig"] = host
	spec["NetworkingConfig"] = obj{"EndpointsConfig": endpoints}
	return spec
}

func copyMap(m obj) obj {
	out := obj{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// Recreate pulls a container's image and replaces the container with an identical one on
// the new image. Compose containers go through docker compose for their service; anything
// else is cloned through the API. Data in volumes and bind mounts is kept. If anything fails
// the old container is put back.
func (c *Client) Recreate(id string) (string, error) {
	var inspect obj
	if err := c.do("GET", "/containers/"+id+"/json", nil, &inspect); err != nil {
		return "", err
	}
	cfg, _ := inspect["Config"].(obj)
	labels, _ := cfg["Labels"].(obj)
	image, _ := cfg["Image"].(string)
	name := strings.TrimPrefix(fmt.Sprint(inspect["Name"]), "/")
	fullID, _ := inspect["Id"].(string)

	if proj, _ := labels[labelProject].(string); proj != "" {
		cs, err := c.Containers()
		if err != nil {
			return "", err
		}
		svc, _ := labels[labelService].(string)
		for _, p := range Projects(cs) {
			if p.Name == proj && p.FilesExist && svc != "" {
				out, err := run.Cmd(15*time.Minute, "docker", composeArgs(p, "pull", svc)...)
				if err != nil {
					return out, err
				}
				out2, err := run.Cmd(15*time.Minute, "docker", composeArgs(p, "up", "-d", "--force-recreate", "--no-deps", svc)...)
				if err != nil {
					return strings.TrimSpace(out + "\n" + out2), err
				}
				// Then the whole project, so services depending on this one (like ones sharing its
				// network through network_mode: service:x) are recreated against the new container.
				out3, err := run.Cmd(15*time.Minute, "docker", composeArgs(p, "up", "-d")...)
				return strings.TrimSpace(out + "\n" + out2 + "\n" + out3), err
			}
		}
	}

	if image == "" || strings.HasPrefix(image, "sha256:") {
		return "", errors.New("this container was started from an image ID, not a name, so there's nothing to pull")
	}
	var log []string
	out, err := run.Cmd(15*time.Minute, "docker", "pull", image)
	log = append(log, out)
	if err != nil {
		return strings.Join(log, "\n"), err
	}
	var img struct {
		Config obj `json:"Config"`
	}
	oldImage, _ := inspect["Image"].(string)
	c.do("GET", "/images/"+oldImage+"/json", nil, &img)
	spec := CloneSpec(inspect, img.Config)

	state, _ := inspect["State"].(obj)
	wasRunning, _ := state["Running"].(bool)
	tmpName := fmt.Sprintf("%s-old-%d", name, time.Now().Unix())

	if err := c.do("POST", "/containers/"+fullID+"/rename", url.Values{"name": {tmpName}}, nil); err != nil {
		return strings.Join(log, "\n"), fmt.Errorf("rename: %w", err)
	}
	restore := func(newID string) {
		if newID != "" {
			c.do("DELETE", "/containers/"+newID, url.Values{"force": {"1"}}, nil)
		}
		c.do("POST", "/containers/"+fullID+"/rename", url.Values{"name": {name}}, nil)
		if wasRunning {
			c.do("POST", "/containers/"+fullID+"/start", nil, nil)
		}
	}
	if wasRunning {
		if err := c.do("POST", "/containers/"+fullID+"/stop", nil, nil); err != nil {
			restore("")
			return strings.Join(log, "\n"), fmt.Errorf("stop: %w", err)
		}
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := c.doBody("POST", "/containers/create", url.Values{"name": {name}}, spec, &created); err != nil {
		restore("")
		return strings.Join(log, "\n"), fmt.Errorf("create: %w (the old container was put back)", err)
	}
	if wasRunning {
		if err := c.do("POST", "/containers/"+created.ID+"/start", nil, nil); err != nil {
			restore(created.ID)
			return strings.Join(log, "\n"), fmt.Errorf("start: %w (the old container was put back)", err)
		}
	}
	if err := c.do("DELETE", "/containers/"+fullID, nil, nil); err != nil {
		log = append(log, "The old container is kept as "+tmpName+": "+err.Error())
	}
	log = append(log, "Recreated "+name+" on the new image; volumes and bind mounts kept.")
	return strings.TrimSpace(strings.Join(log, "\n")), nil
}
