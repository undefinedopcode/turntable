// Command docker is a turntable plugin connector that exposes Docker Engine state
// — containers, images, volumes, networks, and live per-container stats — as
// queryable relations. It is built on the Go plugin SDK
// (github.com/undefinedopcode/turntable-go-sdk/ttplugin) and the official Docker
// Engine SDK (github.com/docker/docker/client), talking to the local daemon over
// the socket / DOCKER_HOST (a `host` option overrides it).
//
// Datasets:
//
//	containers        one row per container (all states)
//	images            one row per local image
//	volumes           one row per volume
//	networks          one row per network
//	container_stats   live stats for each running container (cpu%, mem, net, io)
//
// Build it (its own module; build from this directory) and register it:
//
//	go build -o bin/docker ./examples/plugins/docker
//	# turntable.yaml:
//	#   docker:
//	#     connector: plugin
//	#     command: ["/abs/path/bin/docker"]
//	#     dataset: "*"          # or a single dataset, e.g. containers
//
// Then, for example:
//
//	SELECT name, image, state, status FROM containers WHERE state='running'
//	SELECT name, ROUND(cpu_percent,1) cpu, mem_usage FROM container_stats ORDER BY cpu DESC
//	SELECT image, COUNT(*) n FROM containers GROUP BY image ORDER BY n DESC
package main

import (
	"context"
	"encoding/json"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"

	"github.com/undefinedopcode/turntable-go-sdk/ttplugin"
)

// statsConcurrency bounds how many containers are sampled at once; each sample
// reads two frames from the Docker stats stream (~1s of wall time), so the work
// is I/O-bound and benefits from a small pool.
const statsConcurrency = 8

// statsTimeout caps a whole container_stats scan so a slow/unresponsive daemon
// can't hang a query indefinitely.
const statsTimeout = 20 * time.Second

func main() {
	if err := ttplugin.Serve(ttplugin.Plugin{
		Name: "docker",
		Datasets: map[string]ttplugin.Dataset{
			"containers": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "id", Type: "string"},
					{Name: "name", Type: "string", Nullable: true},
					{Name: "image", Type: "string", Nullable: true},
					{Name: "image_id", Type: "string", Nullable: true},
					{Name: "command", Type: "string", Nullable: true},
					{Name: "state", Type: "string", Nullable: true},
					{Name: "status", Type: "string", Nullable: true},
					{Name: "created", Type: "time", Nullable: true},
					{Name: "ports", Type: "string", Nullable: true},
					{Name: "network_mode", Type: "string", Nullable: true},
					{Name: "size_rw", Type: "int", Nullable: true},
					{Name: "size_root_fs", Type: "int", Nullable: true},
					{Name: "labels", Type: "any", Nullable: true},
				}},
				Rows: containerRows,
			},
			"images": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "id", Type: "string"},
					{Name: "repo_tags", Type: "string", Nullable: true},
					{Name: "repo_digests", Type: "string", Nullable: true},
					{Name: "size", Type: "int", Nullable: true},
					{Name: "shared_size", Type: "int", Nullable: true},
					{Name: "containers", Type: "int", Nullable: true},
					{Name: "created", Type: "time", Nullable: true},
					{Name: "labels", Type: "any", Nullable: true},
				}},
				Rows: imageRows,
			},
			"volumes": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "name", Type: "string"},
					{Name: "driver", Type: "string", Nullable: true},
					{Name: "mountpoint", Type: "string", Nullable: true},
					{Name: "scope", Type: "string", Nullable: true},
					{Name: "created", Type: "time", Nullable: true},
					{Name: "labels", Type: "any", Nullable: true},
					{Name: "options", Type: "any", Nullable: true},
				}},
				Rows: volumeRows,
			},
			"networks": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "id", Type: "string"},
					{Name: "name", Type: "string", Nullable: true},
					{Name: "driver", Type: "string", Nullable: true},
					{Name: "scope", Type: "string", Nullable: true},
					{Name: "internal", Type: "bool", Nullable: true},
					{Name: "attachable", Type: "bool", Nullable: true},
					{Name: "ipv6", Type: "bool", Nullable: true},
					{Name: "created", Type: "time", Nullable: true},
					{Name: "containers_count", Type: "int", Nullable: true},
					{Name: "labels", Type: "any", Nullable: true},
				}},
				Rows: networkRows,
			},
			"container_stats": {
				Schema: ttplugin.Schema{Columns: []ttplugin.Column{
					{Name: "id", Type: "string"},
					{Name: "name", Type: "string", Nullable: true},
					{Name: "cpu_percent", Type: "float", Nullable: true},
					{Name: "mem_usage", Type: "int", Nullable: true},
					{Name: "mem_limit", Type: "int", Nullable: true},
					{Name: "mem_percent", Type: "float", Nullable: true},
					{Name: "net_rx", Type: "int", Nullable: true},
					{Name: "net_tx", Type: "int", Nullable: true},
					{Name: "block_read", Type: "int", Nullable: true},
					{Name: "block_write", Type: "int", Nullable: true},
					{Name: "pids", Type: "int", Nullable: true},
				}},
				Rows: statsRows,
			},
		},
	}); err != nil {
		os.Exit(1)
	}
}

// newClient builds a Docker client from the environment (DOCKER_HOST etc.), with
// API-version negotiation so it works across daemon versions. A `host` option
// overrides the endpoint (e.g. a remote or rootless socket).
func newClient(opts map[string]any) (*client.Client, error) {
	copts := []client.Opt{client.FromEnv, client.WithAPIVersionNegotiation()}
	if h := stringOpt(opts, "host"); h != "" {
		copts = append(copts, client.WithHost(h))
	}
	return client.NewClientWithOpts(copts...)
}

func containerRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cli, err := newClient(req.Options)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	list, err := cli.ContainerList(context.Background(), container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	rows := make(ttplugin.Rows, 0, len(list))
	for _, c := range list {
		rows = append(rows, ttplugin.Row{
			c.ID,
			containerName(c.Names),
			nullStr(c.Image),
			nullStr(c.ImageID),
			nullStr(c.Command),
			nullStr(c.State),
			nullStr(c.Status),
			unixTime(c.Created),
			nullStr(formatPorts(c.Ports)),
			nullStr(c.HostConfig.NetworkMode),
			c.SizeRw,
			c.SizeRootFs,
			labels(c.Labels),
		})
	}
	return rows, nil
}

func imageRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cli, err := newClient(req.Options)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	list, err := cli.ImageList(context.Background(), image.ListOptions{})
	if err != nil {
		return nil, err
	}
	rows := make(ttplugin.Rows, 0, len(list))
	for _, im := range list {
		rows = append(rows, ttplugin.Row{
			im.ID,
			nullStr(strings.Join(im.RepoTags, ", ")),
			nullStr(strings.Join(im.RepoDigests, ", ")),
			im.Size,
			im.SharedSize,
			int(im.Containers),
			unixTime(im.Created),
			labels(im.Labels),
		})
	}
	return rows, nil
}

func volumeRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cli, err := newClient(req.Options)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	resp, err := cli.VolumeList(context.Background(), volume.ListOptions{})
	if err != nil {
		return nil, err
	}
	rows := make(ttplugin.Rows, 0, len(resp.Volumes))
	for _, v := range resp.Volumes {
		if v == nil {
			continue
		}
		rows = append(rows, ttplugin.Row{
			v.Name,
			nullStr(v.Driver),
			nullStr(v.Mountpoint),
			nullStr(v.Scope),
			parseTime(v.CreatedAt),
			labels(v.Labels),
			anyMap(v.Options),
		})
	}
	return rows, nil
}

func networkRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cli, err := newClient(req.Options)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	list, err := cli.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		return nil, err
	}
	rows := make(ttplugin.Rows, 0, len(list))
	for _, n := range list {
		var created any
		if !n.Created.IsZero() {
			created = n.Created
		}
		rows = append(rows, ttplugin.Row{
			n.ID,
			nullStr(n.Name),
			nullStr(n.Driver),
			nullStr(n.Scope),
			n.Internal,
			n.Attachable,
			n.EnableIPv6,
			created,
			len(n.Containers),
			labels(n.Labels),
		})
	}
	return rows, nil
}

// statsRows samples live stats for each running container. Each sample reads two
// frames from the stats stream so a real CPU% (which needs two points) can be
// computed; samples run concurrently under a small worker pool and a whole-scan
// timeout. A container that errors mid-sample is skipped rather than failing the
// scan.
func statsRows(req ttplugin.Request) (ttplugin.Rows, error) {
	cli, err := newClient(req.Options)
	if err != nil {
		return nil, err
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), statsTimeout)
	defer cancel()

	// All:false → running containers only (stats on stopped ones are empty).
	list, err := cli.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		return nil, err
	}

	rows := make(ttplugin.Rows, 0, len(list))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, statsConcurrency)
	for _, c := range list {
		wg.Add(1)
		sem <- struct{}{}
		go func(c types.Container) {
			defer wg.Done()
			defer func() { <-sem }()
			st, err := sampleStats(ctx, cli, c.ID)
			if err != nil {
				return
			}
			row := statRow(c, st)
			mu.Lock()
			rows = append(rows, row)
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	// Stable output regardless of goroutine completion order.
	sort.Slice(rows, func(i, j int) bool { return rows[i][0].(string) < rows[j][0].(string) })
	return rows, nil
}

// sampleStats reads two frames from the container's stats stream and returns the
// second (whose precpu_stats is the first frame, enabling a CPU% delta). If only
// one frame arrives, that frame is returned (CPU% then reads as 0).
func sampleStats(ctx context.Context, cli *client.Client, id string) (*container.StatsResponse, error) {
	resp, err := cli.ContainerStats(ctx, id, true)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	var first, second container.StatsResponse
	if err := dec.Decode(&first); err != nil {
		return nil, err
	}
	if err := dec.Decode(&second); err != nil {
		return &first, nil
	}
	return &second, nil
}

func statRow(c types.Container, s *container.StatsResponse) ttplugin.Row {
	memUsage := memoryUsage(s.MemoryStats)
	memLimit := s.MemoryStats.Limit
	var memPct any
	if memLimit > 0 {
		memPct = float64(memUsage) / float64(memLimit) * 100
	}
	rx, tx := networkTotals(s.Networks)
	read, write := blockIO(s.BlkioStats)
	return ttplugin.Row{
		c.ID,
		containerName(c.Names),
		cpuPercent(s),
		int64(memUsage),
		int64(memLimit),
		memPct,
		int64(rx),
		int64(tx),
		int64(read),
		int64(write),
		int64(s.PidsStats.Current),
	}
}

// cpuPercent computes container CPU utilization from the delta between the frame
// and its predecessor, scaled by the number of online CPUs — the same formula
// `docker stats` uses.
func cpuPercent(s *container.StatsResponse) any {
	cpuDelta := float64(s.CPUStats.CPUUsage.TotalUsage) - float64(s.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(s.CPUStats.SystemUsage) - float64(s.PreCPUStats.SystemUsage)
	online := float64(s.CPUStats.OnlineCPUs)
	if online == 0 {
		online = float64(len(s.CPUStats.CPUUsage.PercpuUsage))
	}
	if sysDelta > 0 && cpuDelta > 0 {
		return (cpuDelta / sysDelta) * online * 100
	}
	return float64(0)
}

// memoryUsage is the container's memory usage with the page cache excluded, as
// `docker stats` reports it (cgroup v2 uses inactive_file; v1 uses cache).
func memoryUsage(m container.MemoryStats) uint64 {
	usage := m.Usage
	if v, ok := m.Stats["inactive_file"]; ok {
		if v < usage {
			return usage - v
		}
		return 0
	}
	if v, ok := m.Stats["cache"]; ok {
		if v < usage {
			return usage - v
		}
		return 0
	}
	return usage
}

func networkTotals(nets map[string]container.NetworkStats) (rx, tx uint64) {
	for _, n := range nets {
		rx += n.RxBytes
		tx += n.TxBytes
	}
	return rx, tx
}

func blockIO(b container.BlkioStats) (read, write uint64) {
	for _, e := range b.IoServiceBytesRecursive {
		switch strings.ToLower(e.Op) {
		case "read":
			read += e.Value
		case "write":
			write += e.Value
		}
	}
	return read, write
}

// ---- value helpers -----------------------------------------------------------

// containerName returns the first name without Docker's leading slash, or NULL.
func containerName(names []string) any {
	if len(names) == 0 {
		return nil
	}
	return strings.TrimPrefix(names[0], "/")
}

// formatPorts renders a container's port mappings like the `docker ps` PORTS
// column, e.g. "0.0.0.0:8080->80/tcp, 443/tcp".
func formatPorts(ports []types.Port) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		s := ""
		if p.PublicPort != 0 {
			host := p.IP
			if host == "" {
				host = "0.0.0.0"
			}
			s = host + ":" + strconv.Itoa(int(p.PublicPort)) + "->"
		}
		s += strconv.Itoa(int(p.PrivatePort)) + "/" + p.Type
		parts = append(parts, s)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}

func labels(m map[string]string) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func anyMap(m map[string]string) any {
	if len(m) == 0 {
		return nil
	}
	return m
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// unixTime maps a Unix-seconds timestamp to a time, or NULL for the zero value.
func unixTime(sec int64) any {
	if sec <= 0 {
		return nil
	}
	return time.Unix(sec, 0).UTC()
}

// parseTime parses an RFC3339 timestamp string (volumes' CreatedAt), or NULL.
func parseTime(s string) any {
	if s == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return nil
}

func stringOpt(opts map[string]any, key string) string {
	if v, ok := opts[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}
