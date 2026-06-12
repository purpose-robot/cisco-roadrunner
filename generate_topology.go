package main

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"os"
	"sort"
	"strings"
)

const (
	inputFilename  = "cisco_cdp_neighbors.csv"
	outputFilename = "network_topology.html"
)

var requiredColumns = []string{
	"location",
	"local_hostname",
	"local_ip_address",
	"local_interface",
	"remote_hostname",
	"remote_platform",
	"remote_ip_address",
	"remote_interface",
}

type csvRow struct {
	Location        string
	LocalHostname   string
	LocalIPAddress  string
	LocalInterface  string
	RemoteHostname  string
	RemotePlatform  string
	RemoteIPAddress string
	RemoteInterface string
}

type nodeRecord struct {
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	IP       string `json:"ip"`
	Location string `json:"location"`
	Platform string `json:"platform"`
	Category string `json:"category"`
	Color    string `json:"color"`
	Weight   int    `json:"weight"`
}

type edgeRecord struct {
	ID            string `json:"id"`
	From          string `json:"from"`
	To            string `json:"to"`
	FromHostname  string `json:"fromHostname"`
	ToHostname    string `json:"toHostname"`
	FromInterface string `json:"fromInterface"`
	ToInterface   string `json:"toInterface"`
}

type edgeCandidate struct {
	AHost      string
	AInterface string
	BHost      string
	BInterface string
}

type platformOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type topologyData struct {
	Nodes     []nodeRecord     `json:"nodes"`
	Edges     []edgeRecord     `json:"edges"`
	Platforms []platformOption `json:"platforms"`
}

type pageData struct {
	TopologyJSON template.JS
}

func main() {
	topology, err := loadTopology(inputFilename)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load topology: %v\n", err)
		os.Exit(1)
	}

	payload, err := json.Marshal(topology)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to encode topology data: %v\n", err)
		os.Exit(1)
	}

	tmpl, err := template.New("network-topology").Parse(pageTemplate)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to parse HTML template: %v\n", err)
		os.Exit(1)
	}

	var output bytes.Buffer
	if err := tmpl.Execute(&output, pageData{TopologyJSON: template.JS(payload)}); err != nil {
		fmt.Fprintf(os.Stderr, "failed to render HTML template: %v\n", err)
		os.Exit(1)
	}

	if err := os.WriteFile(outputFilename, output.Bytes(), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "failed to write %s: %v\n", outputFilename, err)
		os.Exit(1)
	}

	fmt.Printf(
		"wrote %s with %d nodes and %d edges\n",
		outputFilename,
		len(topology.Nodes),
		len(topology.Edges),
	)
}

func loadTopology(filename string) (topologyData, error) {
	file, err := os.Open(filename)
	if err != nil {
		return topologyData{}, fmt.Errorf("open %s: %w", filename, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.TrimLeadingSpace = true

	header, err := reader.Read()
	if err != nil {
		return topologyData{}, fmt.Errorf("read CSV header: %w", err)
	}

	columnIndex, err := validateHeader(header)
	if err != nil {
		return topologyData{}, err
	}
	reader.FieldsPerRecord = len(header)

	nodesByHost := make(map[string]*nodeRecord)
	edgesByKey := make(map[string]edgeCandidate)
	platformsByValue := make(map[string]string)

	for rowNumber := 2; ; rowNumber++ {
		record, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return topologyData{}, fmt.Errorf("read CSV row %d: %w", rowNumber, err)
		}

		row := parseRow(record, columnIndex)
		if isEmptyRow(row) {
			continue
		}
		if row.LocalHostname == "" || row.RemoteHostname == "" {
			return topologyData{}, fmt.Errorf(
				"CSV row %d must contain local_hostname and remote_hostname",
				rowNumber,
			)
		}

		upsertNode(
			nodesByHost,
			row.LocalHostname,
			row.LocalIPAddress,
			row.Location,
			"",
		)
		upsertNode(
			nodesByHost,
			row.RemoteHostname,
			row.RemoteIPAddress,
			row.Location,
			row.RemotePlatform,
		)

		if row.RemotePlatform != "" {
			key := strings.ToLower(row.RemotePlatform)
			if _, exists := platformsByValue[key]; !exists {
				platformsByValue[key] = row.RemotePlatform
			}
		}

		aHost := normalizeHostname(row.LocalHostname)
		bHost := normalizeHostname(row.RemoteHostname)
		aInterface := shortenInterface(row.LocalInterface)
		bInterface := shortenInterface(row.RemoteInterface)

		sideA := edgeSideKey(aHost, aInterface)
		sideB := edgeSideKey(bHost, bInterface)
		candidate := edgeCandidate{
			AHost:      aHost,
			AInterface: aInterface,
			BHost:      bHost,
			BInterface: bInterface,
		}
		if sideB < sideA {
			sideA, sideB = sideB, sideA
			candidate = edgeCandidate{
				AHost:      bHost,
				AInterface: bInterface,
				BHost:      aHost,
				BInterface: aInterface,
			}
		}

		key := sideA + "\x1e" + sideB
		if _, exists := edgesByKey[key]; !exists {
			edgesByKey[key] = candidate
		}
	}

	if len(nodesByHost) == 0 {
		return topologyData{}, fmt.Errorf("%s contains no topology rows", filename)
	}

	nodeKeys := make([]string, 0, len(nodesByHost))
	for key := range nodesByHost {
		nodeKeys = append(nodeKeys, key)
	}
	sort.Slice(nodeKeys, func(i, j int) bool {
		left := nodesByHost[nodeKeys[i]]
		right := nodesByHost[nodeKeys[j]]
		if !strings.EqualFold(left.Location, right.Location) {
			return strings.ToLower(left.Location) < strings.ToLower(right.Location)
		}
		return strings.ToLower(left.Hostname) < strings.ToLower(right.Hostname)
	})

	nodes := make([]nodeRecord, 0, len(nodeKeys))
	for i, key := range nodeKeys {
		node := nodesByHost[key]
		node.ID = fmt.Sprintf("n%d", i+1)
		if node.Location == "" {
			node.Location = "Unknown"
		}
		node.Category, node.Weight, node.Color = classifyPlatform(node.Platform)
		nodes = append(nodes, *node)
	}

	edgeKeys := make([]string, 0, len(edgesByKey))
	for key := range edgesByKey {
		edgeKeys = append(edgeKeys, key)
	}
	sort.Strings(edgeKeys)

	edges := make([]edgeRecord, 0, len(edgeKeys))
	for _, key := range edgeKeys {
		candidate := edgesByKey[key]
		fromNode := nodesByHost[candidate.AHost]
		toNode := nodesByHost[candidate.BHost]
		fromInterface := candidate.AInterface
		toInterface := candidate.BInterface

		if nodeComesFirst(toNode, fromNode) {
			fromNode, toNode = toNode, fromNode
			fromInterface, toInterface = toInterface, fromInterface
		}

		edges = append(edges, edgeRecord{
			ID:            fmt.Sprintf("e%d", len(edges)+1),
			From:          fromNode.ID,
			To:            toNode.ID,
			FromHostname:  fromNode.Hostname,
			ToHostname:    toNode.Hostname,
			FromInterface: valueOrUnknown(fromInterface),
			ToInterface:   valueOrUnknown(toInterface),
		})
	}

	platformKeys := make([]string, 0, len(platformsByValue))
	for key := range platformsByValue {
		platformKeys = append(platformKeys, key)
	}
	sort.Strings(platformKeys)

	platforms := make([]platformOption, 0, len(platformKeys))
	for _, key := range platformKeys {
		value := platformsByValue[key]
		platforms = append(platforms, platformOption{
			Value: value,
			Label: capitalizeFirst(value),
		})
	}

	return topologyData{
		Nodes:     nodes,
		Edges:     edges,
		Platforms: platforms,
	}, nil
}

func validateHeader(header []string) (map[string]int, error) {
	index := make(map[string]int, len(header))
	for i, column := range header {
		column = strings.TrimSpace(strings.TrimPrefix(column, "\ufeff"))
		index[column] = i
	}

	var missing []string
	for _, column := range requiredColumns {
		if _, exists := index[column]; !exists {
			missing = append(missing, column)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("CSV is missing required columns: %s", strings.Join(missing, ", "))
	}

	return index, nil
}

func parseRow(record []string, index map[string]int) csvRow {
	value := func(column string) string {
		return strings.TrimSpace(record[index[column]])
	}

	return csvRow{
		Location:        value("location"),
		LocalHostname:   value("local_hostname"),
		LocalIPAddress:  value("local_ip_address"),
		LocalInterface:  value("local_interface"),
		RemoteHostname:  value("remote_hostname"),
		RemotePlatform:  value("remote_platform"),
		RemoteIPAddress: value("remote_ip_address"),
		RemoteInterface: value("remote_interface"),
	}
}

func isEmptyRow(row csvRow) bool {
	return row.Location == "" &&
		row.LocalHostname == "" &&
		row.LocalIPAddress == "" &&
		row.LocalInterface == "" &&
		row.RemoteHostname == "" &&
		row.RemotePlatform == "" &&
		row.RemoteIPAddress == "" &&
		row.RemoteInterface == ""
}

func upsertNode(
	nodes map[string]*nodeRecord,
	hostname string,
	ip string,
	location string,
	platform string,
) {
	key := normalizeHostname(hostname)
	node, exists := nodes[key]
	if !exists {
		node = &nodeRecord{Hostname: hostname}
		nodes[key] = node
	}

	if node.Hostname == "" {
		node.Hostname = hostname
	}
	if node.IP == "" && ip != "" {
		node.IP = ip
	}
	if node.Location == "" && location != "" {
		node.Location = location
	}
	if node.Platform == "" && platform != "" {
		node.Platform = platform
	}
}

func normalizeHostname(hostname string) string {
	return strings.ToLower(strings.TrimSpace(hostname))
}

func edgeSideKey(hostname string, interfaceName string) string {
	normalizedInterface := strings.ToLower(strings.ReplaceAll(interfaceName, " ", ""))
	return hostname + "\x1f" + normalizedInterface
}

func shortenInterface(interfaceName string) string {
	interfaceName = strings.TrimSpace(interfaceName)
	prefixes := []struct {
		Long  string
		Short string
	}{
		{"TwentyFiveGigabitEthernet", "Twe"},
		{"TwentyFiveGigE", "Twe"},
		{"HundredGigabitEthernet", "Hu"},
		{"HundredGigE", "Hu"},
		{"FortyGigabitEthernet", "Fo"},
		{"TenGigabitEthernet", "Te"},
		{"GigabitEthernet", "Gi"},
		{"FastEthernet", "Fa"},
		{"Port-channel", "Po"},
		{"Ethernet", "Eth"},
	}

	lowerName := strings.ToLower(interfaceName)
	for _, prefix := range prefixes {
		lowerPrefix := strings.ToLower(prefix.Long)
		if strings.HasPrefix(lowerName, lowerPrefix) {
			return prefix.Short + interfaceName[len(prefix.Long):]
		}
	}
	return interfaceName
}

func classifyPlatform(platform string) (category string, weight int, color string) {
	lowerPlatform := strings.ToLower(platform)
	switch {
	case strings.Contains(lowerPlatform, "c9500-"):
		return "CORE", 1, "#e74c3c"
	case strings.Contains(lowerPlatform, "ip phone"),
		strings.Contains(lowerPlatform, "air-"):
		return "CLIENTS", 3, "#2aa876"
	default:
		return "ACCESS", 2, "#2878b5"
	}
}

func nodeComesFirst(left *nodeRecord, right *nodeRecord) bool {
	if left.Weight != right.Weight {
		return left.Weight < right.Weight
	}
	return strings.ToLower(left.Hostname) < strings.ToLower(right.Hostname)
}

func capitalizeFirst(value string) string {
	if value == "" {
		return value
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "Unknown"
	}
	return value
}

const pageTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Cisco CDP Network Topology</title>
  <link rel="stylesheet" href="https://unpkg.com/vis-network@9.1.9/styles/vis-network.min.css">
  <style>
    :root {
      color-scheme: light;
      --ink: #17212b;
      --muted: #667786;
      --line: #d9e1e8;
      --panel: #ffffff;
      --surface: #f3f6f8;
      --header: #102a43;
      --accent: #0f6cbd;
      --shadow: 0 12px 32px rgba(26, 45, 63, 0.10);
    }

    * { box-sizing: border-box; }

    html, body {
      width: 100%;
      height: 100%;
      margin: 0;
      overflow: hidden;
    }

    body {
      display: grid;
      grid-template-rows: 64px minmax(0, 1fr) 34px;
      color: var(--ink);
      background: var(--surface);
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
    }

    header {
      z-index: 5;
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 24px;
      padding: 0 24px;
      color: #fff;
      background: linear-gradient(105deg, #102a43, #164e72);
      box-shadow: 0 2px 12px rgba(16, 42, 67, 0.25);
    }

    header h1 {
      margin: 0;
      font-size: clamp(18px, 2vw, 24px);
      font-weight: 650;
      letter-spacing: 0.01em;
    }

    .legend {
      display: flex;
      align-items: center;
      gap: 14px;
      font-size: 12px;
      color: #dbeafe;
    }

    .legend span {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      white-space: nowrap;
    }

    .legend i {
      width: 10px;
      height: 10px;
      border-radius: 50%;
      box-shadow: 0 0 0 2px rgba(255, 255, 255, 0.25);
    }

    .layout {
      position: relative;
      min-height: 0;
      display: grid;
      grid-template-columns: 250px minmax(0, 1fr) 350px;
      gap: 1px;
      background: var(--line);
    }

    .filters,
    #detail {
      min-width: 0;
      overflow: auto;
      background: var(--panel);
    }

    .filters {
      padding: 22px 18px;
    }

    .sidebar-title {
      margin: 0 0 18px;
      font-size: 15px;
      font-weight: 700;
      letter-spacing: 0.08em;
      text-transform: uppercase;
      color: #334e68;
    }

    .field {
      display: grid;
      gap: 7px;
      margin-bottom: 16px;
    }

    .field label {
      color: #486581;
      font-size: 12px;
      font-weight: 650;
    }

    .field input,
    .field select {
      width: 100%;
      height: 40px;
      border: 1px solid #cbd5df;
      border-radius: 8px;
      padding: 0 11px;
      color: var(--ink);
      background: #fff;
      font: inherit;
      font-size: 13px;
      outline: none;
      transition: border-color 120ms ease, box-shadow 120ms ease;
    }

    .field input:focus,
    .field select:focus {
      border-color: var(--accent);
      box-shadow: 0 0 0 3px rgba(15, 108, 189, 0.14);
    }

    #reset-filters {
      width: 100%;
      min-height: 40px;
      border: 0;
      border-radius: 8px;
      padding: 9px 12px;
      color: #fff;
      background: var(--accent);
      font: inherit;
      font-size: 13px;
      font-weight: 650;
      cursor: pointer;
    }

    #reset-filters:hover { background: #0b5da7; }

    .match-count {
      margin-top: 18px;
      padding: 13px;
      border: 1px solid #d9e7f2;
      border-radius: 9px;
      color: #294861;
      background: #f1f8fd;
      font-size: 13px;
      line-height: 1.45;
      text-align: center;
    }

    .match-count strong {
      display: block;
      margin-bottom: 2px;
      color: #0b5da7;
      font-size: 17px;
    }

    main {
      position: relative;
      min-width: 0;
      min-height: 0;
      overflow: hidden;
      background:
        radial-gradient(circle at 20% 10%, rgba(40, 120, 181, 0.06), transparent 30%),
        #f8fafc;
    }

    #network {
      width: 100%;
      height: 100%;
      outline: none;
    }

    .canvas-hint {
      position: absolute;
      right: 12px;
      bottom: 12px;
      z-index: 2;
      border: 1px solid rgba(203, 213, 223, 0.9);
      border-radius: 7px;
      padding: 7px 9px;
      color: #627587;
      background: rgba(255, 255, 255, 0.88);
      box-shadow: 0 4px 14px rgba(26, 45, 63, 0.08);
      font-size: 11px;
      pointer-events: none;
      backdrop-filter: blur(5px);
    }

    #edge-tooltip {
      position: absolute;
      z-index: 20;
      display: none;
      max-width: 320px;
      border-radius: 8px;
      padding: 9px 11px;
      color: #fff;
      background: rgba(16, 42, 67, 0.96);
      box-shadow: var(--shadow);
      font-size: 12px;
      line-height: 1.45;
      pointer-events: none;
    }

    #edge-tooltip .tooltip-hosts {
      margin-bottom: 3px;
      font-weight: 700;
    }

    #edge-tooltip .tooltip-interfaces {
      color: #dbeafe;
      font-family: ui-monospace, SFMono-Regular, Consolas, monospace;
    }

    #detail {
      position: relative;
      border-left: 0 solid var(--accent);
      transition: border-width 150ms ease, transform 180ms ease;
    }

    #detail.open { border-left-width: 4px; }

    #detail-body {
      min-height: 100%;
      padding: 22px 18px;
    }

    #detail-close {
      position: absolute;
      top: 12px;
      right: 12px;
      width: 30px;
      height: 30px;
      border: 0;
      border-radius: 50%;
      color: #526777;
      background: #edf2f6;
      font-size: 21px;
      line-height: 28px;
      cursor: pointer;
    }

    #detail-close:hover {
      color: #17324d;
      background: #dfe8ef;
    }

    #detail-content h2 {
      margin: 2px 38px 20px 0;
      overflow-wrap: anywhere;
      color: #17324d;
      font-size: 20px;
      line-height: 1.25;
    }

    #detail-content dl { margin: 0; }

    #detail-content dt {
      margin-top: 15px;
      color: #728493;
      font-size: 11px;
      font-weight: 700;
      letter-spacing: 0.06em;
      text-transform: uppercase;
    }

    #detail-content dd {
      margin: 5px 0 0;
      color: #243b53;
      font-size: 13px;
      line-height: 1.45;
    }

    .type-badge {
      display: inline-block;
      border-radius: 999px;
      padding: 3px 8px;
      color: #fff;
      font-size: 10px;
      font-weight: 750;
      letter-spacing: 0.05em;
      vertical-align: 1px;
    }

    .interface-table-wrap {
      overflow-x: auto;
      margin-top: 8px;
      border: 1px solid #dce4eb;
      border-radius: 8px;
    }

    #detail-content table {
      width: 100%;
      border-collapse: collapse;
      font-size: 11px;
    }

    #detail-content th,
    #detail-content td {
      padding: 8px 7px;
      border-bottom: 1px solid #e7edf2;
      text-align: left;
      white-space: nowrap;
    }

    #detail-content th {
      color: #486581;
      background: #f5f8fa;
      font-weight: 700;
    }

    #detail-content tr:last-child td { border-bottom: 0; }

    .detail-empty {
      display: grid;
      place-items: center;
      min-height: 240px;
      padding: 28px;
      color: #738797;
      text-align: center;
      line-height: 1.55;
    }

    footer {
      display: flex;
      align-items: center;
      justify-content: flex-end;
      padding: 0 18px;
      color: #718096;
      background: #e9eef2;
      font-size: 11px;
      text-align: right;
    }

    @media (max-width: 1100px) {
      .layout { grid-template-columns: 220px minmax(0, 1fr); }

      #detail {
        position: absolute;
        z-index: 10;
        top: 0;
        right: 0;
        bottom: 0;
        width: min(350px, 44vw);
        box-shadow: -12px 0 32px rgba(26, 45, 63, 0.16);
        transform: translateX(102%);
      }

      #detail.open { transform: translateX(0); }
    }

    @media (max-width: 760px) {
      body { grid-template-rows: 58px minmax(0, 1fr) 30px; }
      header { padding: 0 14px; }
      .legend { display: none; }
      .layout { grid-template-columns: 190px minmax(0, 1fr); }
      .filters { padding: 16px 12px; }
      #detail { width: min(360px, 72vw); }
      .canvas-hint { display: none; }
    }
  </style>
</head>
<body>
  <header>
    <h1>Cisco CDP Network Topology</h1>
    <div class="legend" aria-label="Device category legend">
      <span><i style="background:#e74c3c"></i>CORE</span>
      <span><i style="background:#2878b5"></i>ACCESS</span>
      <span><i style="background:#2aa876"></i>CLIENTS</span>
    </div>
  </header>

  <div class="layout">
    <section class="filters" aria-label="Topology filters">
      <h2 class="sidebar-title">Filters</h2>
      <div class="field">
        <label for="hostname-filter">Hostname</label>
        <input id="hostname-filter" type="search" placeholder="e.g. DE-BE-CORE" autocomplete="off">
      </div>
      <div class="field">
        <label for="ip-filter">IP address</label>
        <input id="ip-filter" type="search" placeholder="e.g. 10.10." autocomplete="off">
      </div>
      <div class="field">
        <label for="platform-filter">Device type</label>
        <select id="platform-filter">
          <option value="">All device types</option>
        </select>
      </div>
      <button id="reset-filters" type="button">Reset filters</button>
      <div class="match-count" aria-live="polite">
        <strong id="match-count-value">0 nodes &middot; 0 edges</strong>
        matched in current filters
      </div>
    </section>

    <main>
      <div id="network" aria-label="Interactive Cisco CDP network topology"></div>
      <div id="edge-tooltip" role="tooltip"></div>
      <div class="canvas-hint">Drag to pan &middot; Scroll to zoom &middot; Click a device for details</div>
    </main>

    <aside id="detail" aria-label="Node details">
      <div id="detail-body">
        <button id="detail-close" type="button" title="Close" aria-label="Close details">&times;</button>
        <div id="detail-content">
          <div class="detail-empty">Select a device to inspect its properties and connected interfaces.</div>
        </div>
      </div>
    </aside>
  </div>

  <footer>Footer placeholder</footer>

  <script src="https://unpkg.com/vis-network@9.1.9/dist/vis-network.min.js"></script>
  <script>
    (() => {
      "use strict";

      const topology = {{.TopologyJSON}};
      const networkElement = document.getElementById("network");
      const tooltip = document.getElementById("edge-tooltip");
      const detail = document.getElementById("detail");
      const detailContent = document.getElementById("detail-content");
      const hostnameFilter = document.getElementById("hostname-filter");
      const ipFilter = document.getElementById("ip-filter");
      const platformFilter = document.getElementById("platform-filter");
      const matchCount = document.getElementById("match-count-value");

      if (!window.vis || !window.vis.Network) {
        networkElement.innerHTML = '<div class="detail-empty">Unable to load vis-network from the CDN.</div>';
        return;
      }

      const collator = new Intl.Collator(undefined, { numeric: true, sensitivity: "base" });
      const nodeByID = new Map(topology.nodes.map((node) => [node.id, node]));
      const edgeByID = new Map(topology.edges.map((edge) => [edge.id, edge]));
      const clusterBounds = [];

      topology.platforms.forEach((platform) => {
        const option = document.createElement("option");
        option.value = platform.value;
        option.textContent = platform.label;
        platformFilter.appendChild(option);
      });

      function layoutNodes(nodes) {
        const categoryOrder = ["CORE", "ACCESS", "CLIENTS"];
        const locations = [...new Set(nodes.map((node) => node.location))]
          .sort((a, b) => collator.compare(a, b));
        const laneWidth = 1700;
        const laneGap = 240;
        const nodeGapX = 165;
        const nodeGapY = 118;
        const categoryGap = 125;
        const maxColumns = 9;

        locations.forEach((location, locationIndex) => {
          const centerX = locationIndex * (laneWidth + laneGap);
          let cursorY = 0;

          categoryOrder.forEach((category) => {
            const group = nodes
              .filter((node) => node.location === location && node.category === category)
              .sort((a, b) => collator.compare(a.hostname, b.hostname));

            if (group.length === 0) {
              return;
            }

            const rowCount = Math.ceil(group.length / maxColumns);
            for (let row = 0; row < rowCount; row += 1) {
              const rowNodes = group.slice(row * maxColumns, (row + 1) * maxColumns);
              const rowWidth = (rowNodes.length - 1) * nodeGapX;
              rowNodes.forEach((node, column) => {
                node.x = centerX - (rowWidth / 2) + (column * nodeGapX);
                node.y = cursorY + (row * nodeGapY);
              });
            }
            cursorY += (rowCount * nodeGapY) + categoryGap;
          });

          clusterBounds.push({
            location,
            left: centerX - (laneWidth / 2),
            right: centerX + (laneWidth / 2),
            top: -145,
            bottom: Math.max(cursorY - categoryGap + 145, 220)
          });
        });
      }

      layoutNodes(topology.nodes);

      const parallelEdges = new Map();
      topology.edges.forEach((edge) => {
        const key = [edge.from, edge.to].sort().join("|");
        if (!parallelEdges.has(key)) {
          parallelEdges.set(key, []);
        }
        parallelEdges.get(key).push(edge);
      });

      const visNodes = topology.nodes.map((node) => ({
        id: node.id,
        label: node.hostname,
        x: node.x,
        y: node.y,
        level: node.weight,
        shape: "box",
        margin: { top: 9, right: 11, bottom: 9, left: 11 },
        widthConstraint: { maximum: 160 },
        color: {
          background: node.color,
          border: darken(node.color, 0.20),
          highlight: { background: node.color, border: "#102a43" },
          hover: { background: node.color, border: "#102a43" }
        },
        font: { color: "#ffffff", size: 13, face: "Inter, Segoe UI, sans-serif" },
        borderWidth: 1.5,
        borderWidthSelected: 3,
        shadow: { enabled: true, color: "rgba(16,42,67,0.16)", size: 8, x: 0, y: 3 }
      }));

      const visEdges = topology.edges.map((edge) => {
        const key = [edge.from, edge.to].sort().join("|");
        const siblings = parallelEdges.get(key);
        const siblingIndex = siblings.findIndex((candidate) => candidate.id === edge.id);
        let smooth = false;
        if (siblings.length > 1) {
          smooth = {
            enabled: true,
            type: siblingIndex % 2 === 0 ? "curvedCW" : "curvedCCW",
            roundness: 0.10 + (Math.floor(siblingIndex / 2) * 0.08)
          };
        }

        return {
          id: edge.id,
          from: edge.from,
          to: edge.to,
          smooth,
          width: 1.8,
          color: {
            color: "rgba(76, 100, 122, 0.72)",
            highlight: "#0f6cbd",
            hover: "#0f6cbd",
            inherit: false
          },
          selectionWidth: 1.2,
          hoverWidth: 0.8
        };
      });

      const nodes = new vis.DataSet(visNodes);
      const edges = new vis.DataSet(visEdges);
      const network = new vis.Network(
        networkElement,
        { nodes, edges },
        {
          autoResize: true,
          layout: { improvedLayout: false },
          physics: false,
          interaction: {
            hover: true,
            hoverConnectedEdges: false,
            multiselect: false,
            navigationButtons: false,
            keyboard: true,
            tooltipDelay: 150
          },
          nodes: { chosen: true },
          edges: { chosen: true }
        }
      );

      network.on("beforeDrawing", (context) => {
        clusterBounds.forEach((bounds) => {
          const width = bounds.right - bounds.left;
          const height = bounds.bottom - bounds.top;
          context.save();
          context.fillStyle = "rgba(255, 255, 255, 0.72)";
          context.strokeStyle = "rgba(108, 132, 150, 0.32)";
          context.lineWidth = 2;
          context.setLineDash([10, 8]);
          roundedRect(context, bounds.left, bounds.top, width, height, 28);
          context.fill();
          context.stroke();
          context.setLineDash([]);
          context.fillStyle = "#486581";
          context.font = "700 24px Inter, Segoe UI, sans-serif";
          context.textAlign = "left";
          context.fillText(bounds.location, bounds.left + 32, bounds.top + 43);
          context.restore();
        });
      });

      network.once("afterDrawing", () => {
        network.fit({ animation: { duration: 450, easingFunction: "easeInOutQuad" } });
      });

      let pointer = { x: 0, y: 0 };
      let hoveredEdge = null;

      networkElement.addEventListener("mousemove", (event) => {
        const rect = networkElement.getBoundingClientRect();
        pointer = { x: event.clientX - rect.left, y: event.clientY - rect.top };
        if (hoveredEdge) {
          positionTooltip();
        }
      });

      network.on("hoverEdge", (params) => {
        hoveredEdge = params.edge;
        const edge = edgeByID.get(params.edge);
        tooltip.innerHTML =
          '<div class="tooltip-hosts">' +
          escapeHTML(edge.fromHostname) + " &harr; " + escapeHTML(edge.toHostname) +
          '</div><div class="tooltip-interfaces">' +
          escapeHTML(edge.fromInterface) + " &lt;-&gt; " + escapeHTML(edge.toInterface) +
          "</div>";
        tooltip.style.display = "block";
        positionTooltip();
      });

      network.on("blurEdge", () => {
        hoveredEdge = null;
        tooltip.style.display = "none";
      });

      network.on("click", (params) => {
        if (params.nodes.length === 1) {
          showNodeDetails(params.nodes[0]);
        }
      });

      function positionTooltip() {
        const maxX = Math.max(8, networkElement.clientWidth - tooltip.offsetWidth - 8);
        const maxY = Math.max(8, networkElement.clientHeight - tooltip.offsetHeight - 8);
        tooltip.style.left = Math.min(pointer.x + 14, maxX) + "px";
        tooltip.style.top = Math.min(pointer.y + 14, maxY) + "px";
      }

      function showNodeDetails(nodeID) {
        const node = nodeByID.get(nodeID);
        const connections = topology.edges
          .filter((edge) => edge.from === nodeID || edge.to === nodeID)
          .map((edge) => {
            const isFrom = edge.from === nodeID;
            return {
              local: isFrom ? edge.fromInterface : edge.toInterface,
              peer: isFrom ? edge.toHostname : edge.fromHostname,
              peerInterface: isFrom ? edge.toInterface : edge.fromInterface
            };
          })
          .sort((a, b) => {
            const localResult = collator.compare(a.local, b.local);
            return localResult !== 0 ? localResult : collator.compare(a.peer, b.peer);
          });

        const rows = connections.length > 0
          ? connections.map((connection) =>
              "<tr><td>" + escapeHTML(connection.local) +
              "</td><td>" + escapeHTML(connection.peer) +
              "</td><td>" + escapeHTML(connection.peerInterface) + "</td></tr>"
            ).join("")
          : '<tr><td colspan="3">No connected interfaces</td></tr>';

        detailContent.innerHTML =
          "<h2>" + escapeHTML(node.hostname) + "</h2>" +
          "<dl>" +
            "<dt>IP address</dt><dd>" + escapeHTML(node.ip || "Unknown") + "</dd>" +
            "<dt>Location</dt><dd>" + escapeHTML(node.location || "Unknown") + "</dd>" +
            "<dt>Device type</dt><dd>" +
              '<span class="type-badge" style="background:' + escapeHTML(node.color) + '">' +
                escapeHTML(node.category) +
              "</span>&nbsp; " + escapeHTML(capitalizeFirst(node.platform || "Unknown")) +
            "</dd>" +
            "<dt>Connected interfaces (" + connections.length + ")</dt>" +
            '<dd><div class="interface-table-wrap"><table><thead><tr>' +
              "<th>Local</th><th>Peer</th><th>Peer if</th>" +
            "</tr></thead><tbody>" + rows + "</tbody></table></div></dd>" +
          "</dl>";

        detail.classList.add("open");
      }

      document.getElementById("detail-close").addEventListener("click", () => {
        detail.classList.remove("open");
        detailContent.innerHTML =
          '<div class="detail-empty">Select a device to inspect its properties and connected interfaces.</div>';
        network.unselectAll();
      });

      [hostnameFilter, ipFilter].forEach((input) => {
        input.addEventListener("input", applyFilters);
      });
      platformFilter.addEventListener("change", applyFilters);

      document.getElementById("reset-filters").addEventListener("click", () => {
        hostnameFilter.value = "";
        ipFilter.value = "";
        platformFilter.value = "";
        applyFilters();
      });

      function applyFilters() {
        const hostnameQuery = hostnameFilter.value.trim().toLowerCase();
        const ipQuery = ipFilter.value.trim().toLowerCase();
        const platformQuery = platformFilter.value.trim().toLowerCase();
        const active = hostnameQuery !== "" || ipQuery !== "" || platformQuery !== "";
        const matchingNodes = new Set();

        topology.nodes.forEach((node) => {
          const matchesHostname = hostnameQuery === "" ||
            node.hostname.toLowerCase().includes(hostnameQuery);
          const matchesIP = ipQuery === "" ||
            (node.ip || "").toLowerCase().includes(ipQuery);
          const matchesPlatform = platformQuery === "" ||
            (node.platform || "").toLowerCase() === platformQuery;
          if (matchesHostname && matchesIP && matchesPlatform) {
            matchingNodes.add(node.id);
          }
        });

        const emphasizedNodes = new Set(matchingNodes);
        const emphasizedEdges = new Set();
        if (active) {
          topology.edges.forEach((edge) => {
            if (matchingNodes.has(edge.from) || matchingNodes.has(edge.to)) {
              emphasizedEdges.add(edge.id);
              emphasizedNodes.add(edge.from);
              emphasizedNodes.add(edge.to);
            }
          });
        } else {
          topology.edges.forEach((edge) => emphasizedEdges.add(edge.id));
          topology.nodes.forEach((node) => emphasizedNodes.add(node.id));
        }

        nodes.update(topology.nodes.map((node) => {
          const fullOpacity = emphasizedNodes.has(node.id);
          const alpha = fullOpacity ? 1 : 0.20;
          return {
            id: node.id,
            color: {
              background: withAlpha(node.color, alpha),
              border: withAlpha(darken(node.color, 0.20), alpha),
              highlight: { background: node.color, border: "#102a43" },
              hover: { background: node.color, border: "#102a43" }
            },
            font: {
              color: fullOpacity ? "#ffffff" : "rgba(255,255,255,0.28)"
            },
            shadow: fullOpacity
              ? { enabled: true, color: "rgba(16,42,67,0.16)", size: 8, x: 0, y: 3 }
              : { enabled: false }
          };
        }));

        edges.update(topology.edges.map((edge) => {
          const fullOpacity = emphasizedEdges.has(edge.id);
          return {
            id: edge.id,
            color: {
              color: fullOpacity ? "rgba(76,100,122,0.72)" : "rgba(76,100,122,0.14)",
              highlight: "#0f6cbd",
              hover: "#0f6cbd",
              inherit: false
            },
            width: fullOpacity ? 1.8 : 1
          };
        }));

        const edgeCount = active ? emphasizedEdges.size : topology.edges.length;
        matchCount.textContent = matchingNodes.size + " nodes / " + edgeCount + " edges";
      }

      function roundedRect(context, x, y, width, height, radius) {
        const r = Math.min(radius, width / 2, height / 2);
        context.beginPath();
        context.moveTo(x + r, y);
        context.arcTo(x + width, y, x + width, y + height, r);
        context.arcTo(x + width, y + height, x, y + height, r);
        context.arcTo(x, y + height, x, y, r);
        context.arcTo(x, y, x + width, y, r);
        context.closePath();
      }

      function withAlpha(hex, alpha) {
        const value = hex.replace("#", "");
        const red = parseInt(value.slice(0, 2), 16);
        const green = parseInt(value.slice(2, 4), 16);
        const blue = parseInt(value.slice(4, 6), 16);
        return "rgba(" + red + "," + green + "," + blue + "," + alpha + ")";
      }

      function darken(hex, amount) {
        const value = hex.replace("#", "");
        const channels = [0, 2, 4].map((offset) =>
          Math.max(0, Math.round(parseInt(value.slice(offset, offset + 2), 16) * (1 - amount)))
        );
        return "#" + channels.map((channel) => channel.toString(16).padStart(2, "0")).join("");
      }

      function capitalizeFirst(value) {
        return value ? value.charAt(0).toUpperCase() + value.slice(1) : value;
      }

      function escapeHTML(value) {
        return String(value).replace(/[&<>"']/g, (character) => ({
          "&": "&amp;",
          "<": "&lt;",
          ">": "&gt;",
          '"': "&quot;",
          "'": "&#039;"
        })[character]);
      }

      applyFilters();
    })();
  </script>
</body>
</html>
`
