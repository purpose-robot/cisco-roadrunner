package main

import (
	"embed"
	"encoding/csv"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/scrapli/scrapligo/driver/options"
	"github.com/scrapli/scrapligo/logging"
	"github.com/scrapli/scrapligo/platform"
	"github.com/scrapli/scrapligo/response"
	"github.com/sirikothe/gotextfsm"
	"golang.org/x/term"
)

//go:embed templates/*.textfsm
var templates embed.FS

func main() {
	// Fetch command-line arguments.
	debug := flag.Bool("debug", false, "Set log level to debug, default is 'info'")
	location := flag.String("location", "", "Specify location devices are located")
	username := flag.String("username", "", "Specify IP address that should be used")
	ipAddress := flag.String("ipAddress", "", "Specify hostname that should be used")

	flag.Parse()

	if *ipAddress == "" || *username == "" || *location == "" {
		log.Fatal("failed to fetch command-line flags; missing flags")
	}

	fmt.Print("Password: ")
	parsedPassword, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()

	if err != nil {
		log.Fatalf("failed to fetch password from command-line; %v", err)
	}
	if len(string(parsedPassword)) == 0 {
		log.Fatal("failed to fetch password from command-line; no password")
	}

	// Track which network device was already checked.
	visited := make(map[string]bool)

	password := string(parsedPassword)

	// Call function to retrieve information from network device.
	err = fetchInformationFromDevice(*debug, *ipAddress, *username, password, *location, visited)
	if err != nil {
		log.Fatalf("failed to retrieve information from network device: %s; %v", *ipAddress, err)
	}
}

func strVal(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}

	s, ok := v.(string)
	if !ok {
		return ""
	}

	return s
}

func parseTemplate(name string, response *response.Response) ([]map[string]any, error) {
	parsedTemplate, err := templates.ReadFile("templates/" + name)
	if err != nil {
		return nil, fmt.Errorf("failed to read TextFSM template %s from embedded view; %v", name, err)
	}

	fsm := gotextfsm.TextFSM{}
	if err := fsm.ParseString(string(parsedTemplate)); err != nil {
		return nil, fmt.Errorf("failed to parse TextFSM template %s from embedded view; %v", name, err)
	}

	parser := gotextfsm.ParserOutput{}
	if err := parser.ParseTextString(response.Result, fsm, true); err != nil {
		return nil, fmt.Errorf("failed to parse information from provided network device output; %v", err)
	}

	return parser.Dict, nil
}

func fetchInformationFromDevice(debug bool, localIpAddress, username, password, location string, visited map[string]bool) error {
	// Check if current hostname was already checked.
	if visited[localIpAddress] {
		log.Printf("host %s already visited\n", localIpAddress)
		return nil
	}

	visited[localIpAddress] = true

	// --------------------------------------------------------------------------------------------------------------------------

	// Define custom logging instance which should be used.
	logLevel := logging.Info
	if debug {
		logLevel = logging.Debug
	}

	logger, err := logging.NewInstance(
		logging.WithLevel(logLevel), logging.WithLogger(log.Print),
	)
	if err != nil {
		return fmt.Errorf("failed to create custom logging instance; %v", err)
	}

	// --------------------------------------------------------------------------------------------------------------------------

	client, err := platform.NewPlatform(
		"cisco_iosxe",
		localIpAddress,
		options.WithPort(22),
		options.WithLogger(logger),
		options.WithAuthUsername(username),
		options.WithAuthPassword(password),
		options.WithAuthNoStrictKey(),
		options.WithTransportType("standard"),
	)
	if err != nil {
		return fmt.Errorf("failed to initiate new scrapli cli client; %v", err)
	}

	driver, err := client.GetNetworkDriver()
	if err != nil {
		return fmt.Errorf("failed to initiate new scrapli network driver; %v", err)
	}

	err = driver.Open()
	if err != nil {
		return fmt.Errorf("failed to initiate new connection to network driver; %v", err)
	}
	defer driver.Close()

	// --------------------------------------------------------------------------------------------------------------------------

	showVersionOut, err := driver.SendCommand("show version")
	if err != nil {
		return fmt.Errorf("command 'show version' failed; %v", err)
	}

	showInventoryOut, err := driver.SendCommand("show inventory")
	if err != nil {
		return fmt.Errorf("command 'show inventory' failed; %v", err)
	}

	showCdpNeighborOut, err := driver.SendCommand("show cdp neighbor detail")
	if err != nil {
		return fmt.Errorf("command 'show cdp neighbor detail' failed; %v", err)
	}

	// --------------------------------------------------------------------------------------------------------------------------

	showVersionParsed, err := parseTemplate("01_cisco_ios_show_version.textfsm", showVersionOut)
	if err != nil {
		return err
	}

	showInventoryParsed, err := parseTemplate("02_cisco_ios_show_inventory.textfsm", showInventoryOut)
	if err != nil {
		return err
	}

	showCdpNeighborParsed, err := parseTemplate("03_cisco_ios_show_cdp_neighbors_detail.textfsm", showCdpNeighborOut)
	if err != nil {
		return err
	}

	// --------------------------------------------------------------------------------------------------------------------------

	localHostname := strVal(showVersionParsed[0], "HOSTNAME")

	// --------------------------------------------------------------------------------------------------------------------------

	cdpNeighborsFilename := "cisco_cdp_neighbors.csv"

	cdpNeighborsFile, err := os.OpenFile(cdpNeighborsFilename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to create CSV output file %s; %v", cdpNeighborsFilename, err)
	}
	defer cdpNeighborsFile.Close()

	cdpNeighborsWriter := csv.NewWriter(cdpNeighborsFile)
	defer cdpNeighborsWriter.Flush()

	for _, neighbor := range showCdpNeighborParsed {
		remoteHostname := strVal(neighbor, "NEIGHBOR_NAME")
		remoteIpAddress := strVal(neighbor, "MGMT_ADDRESS")

		err = cdpNeighborsWriter.Write(
			[]string{
				location,
				localHostname,
				localIpAddress,
				strVal(neighbor, "LOCAL_INTERFACE"),
				remoteHostname,
				remoteIpAddress,
				strVal(neighbor, "NEIGHBOR_INTERFACE"),
			},
		)
		if err != nil {
			return fmt.Errorf("failed to append data to CSV output file %s; %v", remoteHostname, err)
		}

		if remoteIpAddress == "" {
			log.Printf("skipping discovery for %s: invalid or missing IP address", remoteHostname)
			continue
		}

		err = fetchInformationFromDevice(debug, remoteIpAddress, username, password, location, visited)
		if err != nil {
			log.Printf("failed to retrieve information from network device: %s; %v", remoteHostname, err)
		}
	}

	// --------------------------------------------------------------------------------------------------------------------------

	serialNumbersFilename := "cisco_serial_numbers.csv"

	serialNumbersFile, err := os.OpenFile(serialNumbersFilename, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("failed to create CSV output file %s; %v", serialNumbersFilename, err)
	}
	defer serialNumbersFile.Close()

	serialNumberWriter := csv.NewWriter(serialNumbersFile)
	defer serialNumberWriter.Flush()

	err = serialNumberWriter.Write(
		[]string{
			location,
			localHostname,
			localIpAddress,
			strVal(showInventoryParsed[0], "SN"),
			strVal(showInventoryParsed[0], "PID"),
			strVal(showVersionParsed[0], "VERSION"),
		},
	)
	if err != nil {
		return fmt.Errorf("failed to append data to CSV output file %s; %v", localHostname, err)
	}

	// --------------------------------------------------------------------------------------------------------------------------

	return nil
}
