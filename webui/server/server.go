// TODO
//
// Version 3.1
//
// Cleaned up some extraneous logging.
// Profile menu kind of works now, but doesn't work completely yet.
// Need to check profile switching logic.
// Save Thresholds button works once, but then not again...?
// Also, still can't connect to localhost:5000.
//
// Version 3
//
// Made some progress on trying to ensure that serial communication is non blocking,
//  but localhost:5000 connections stopped working.
// Remove NoSerial flag for now until main functionality is verified.
//
// Version 2
//
// The current version has refactored serial handling to be more robust.
//   But it broke the initial threshold request from the device and some other
//   serial related issues came up.
//  I just got the serial port working again, but I need to test the threshold
//   saving and loading.  I haven't tested it since getting the serial communication
//   working again.  The plot and threshold screens are both working.
//  I think the "save thresholds" button needs attention.  Hopefully  it just needs
//   logging added, but it may be broken atm.
//  I also know the profile menu is not working as intended right now.
//  Besides that, I also need to test how it handles the USB device being unplugged
//   and reconnected.
//
// Version 1
//
//  - the serial port should be closed properly on shutdown.
//  -  thresholds need to be saved properly.  currently they are saved one at a time.
//  -  make sure the react app and server are in sync with the actual thresholds being used.
//     seems like the react app is always using the set of thresholds from when the server
//     started, not the current ones.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/tarm/serial"
)

type ProfileHandler struct {
	Filename   string
	Profiles   map[string][]int
	CurProfile string
	NumSensors int
	Mutex      sync.Mutex
}

type SerialHandler struct {
	Port       string
	BaudRate   int
	SerialPort *serial.Port
	Profile    *ProfileHandler
	NumSensors int
	Mutex      sync.Mutex
	writeQueue chan string
}

var sensor_numbers []int

func init() {
	sensor_numbers = make([]int, NUM_SENSORS)
	for i := range sensor_numbers {
		sensor_numbers[i] = i
	}
}

// Clients holds all currently connected websocket clients
type Client struct {
	conn *websocket.Conn
	send chan []any
}

var (
	// Number of sensors (default 4, can be overridden by flag)
	NUM_SENSORS = 4

	// Global list of all active clients
	clients    = make(map[*Client]bool)
	clientsMux sync.Mutex

	// List of active websocket connections
	activeWebSockets []*websocket.Conn
	wsLock           sync.Mutex

	// Shutdown signal channel
	shutdownSignal = make(chan struct{})

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		EnableCompression: true,
	}

	// Global handlers for WebSocket handlers to access
	profileHandler *ProfileHandler
	serialHandler  *SerialHandler

	// Build directory for static files
	buildDir = filepath.Join(filepath.Dir(filepath.Dir(os.Args[0])), "build")
)

func onStartup() {
	log.Println("[STARTUP] Loading profiles...")
	profileHandler.LoadProfiles()

	// Ensure we have at least an empty profile
	if len(profileHandler.GetProfileNames()) == 0 {
		log.Println("[STARTUP] No profiles found, creating default profile")
		emptyProfile := make([]int, profileHandler.NumSensors)
		profileHandler.AddProfile("Default", emptyProfile)
	}

	log.Println("[STARTUP] Starting serial communication handlers...")
	// Start serial read/write goroutines immediately
	go serialHandler.write()
	go serialHandler.ReadLoop()

	// Wait a moment for the serial connection to stabilize
	time.Sleep(100 * time.Millisecond)

	log.Println("[STARTUP] Requesting initial thresholds from device...")
	// Request current thresholds from device
	trySendSerial("t\n")
}

func onShutdown() {
	log.Println("Cleaning up connections...")

	// Close all websocket connections
	wsLock.Lock()
	for _, ws := range activeWebSockets {
		ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "Server shutdown"))
		ws.Close()
	}
	activeWebSockets = nil
	wsLock.Unlock()

	// Close all client connections
	clientsMux.Lock()
	for client := range clients {
		client.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "Server shutdown"))
		client.conn.Close()
		delete(clients, client)
	}
	clientsMux.Unlock()

	// Close serial connection
	if serialHandler.SerialPort != nil {
		serialHandler.SerialPort.Close()
	}

	// Signal shutdown to all goroutines
	close(shutdownSignal)
}

func NewProfileHandler(filename string, numSensors int) *ProfileHandler {
	return &ProfileHandler{
		Filename:   filename,
		Profiles:   make(map[string][]int),
		CurProfile: "",
		NumSensors: numSensors,
	}
}

func (p *ProfileHandler) LoadProfiles() {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if _, err := os.Stat(p.Filename); os.IsNotExist(err) {
		file, _ := os.Create(p.Filename)
		file.Close()
		return
	}

	data, err := os.ReadFile(p.Filename)
	if err != nil {
		log.Println("Error reading profiles file:", err)
		return
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) == p.NumSensors+1 {
			name := parts[0]
			thresholds := make([]int, p.NumSensors)
			for i := 0; i < p.NumSensors; i++ {
				thresholds[i], _ = strconv.Atoi(parts[i+1])
			}
			p.Profiles[name] = thresholds
			if p.CurProfile == "" {
				p.CurProfile = name
			}
		}
	}
}

func (p *ProfileHandler) GetCurrentThresholds() []int {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if thresholds, ok := p.Profiles[p.CurProfile]; ok {
		// Make a copy to avoid returning the internal slice
		result := make([]int, len(thresholds))
		copy(result, thresholds)
		return result
	}
	return make([]int, p.NumSensors) // Initialize with empty slice instead of nil
}

// GetThresholds returns all threshold values sorted in ascending order
func (p *ProfileHandler) GetThresholds() []int {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	thresholds := make([]int, 0) // Initialize with empty slice instead of nil
	seenThresholds := make(map[int]bool)

	// Collect unique thresholds from all profiles
	for _, profileThresholds := range p.Profiles {
		for _, t := range profileThresholds {
			if !seenThresholds[t] {
				thresholds = append(thresholds, t)
				seenThresholds[t] = true
			}
		}
	}

	sort.Ints(thresholds)
	return thresholds
}

func (p *ProfileHandler) UpdateThreshold(index int, value int) {
	if p.CurProfile != "" {
		// Get current thresholds, update the specific index, and do a bulk update
		thresholds := p.GetCurrentThresholds()
		if index >= 0 && index < len(thresholds) {
			thresholds[index] = value
			p.UpdateAllThresholds(thresholds)
		}
	}
}

func (p *ProfileHandler) AddProfile(name string, thresholds []int) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	p.Profiles[name] = thresholds
	if p.CurProfile == "" {
		p.Profiles[""] = make([]int, p.NumSensors)
	}
	p.CurProfile = name
	p.saveProfiles()

	broadcastMessage([]any{"thresholds", map[string]any{
		"thresholds": p.GetCurrentThresholds(),
	}})
	broadcastMessage([]any{"get_profiles", map[string]any{
		"profiles": p.GetProfileNames(),
	}})
	broadcastMessage([]any{"get_cur_profile", map[string]any{
		"cur_profile": p.CurProfile,
	}})
}

func (p *ProfileHandler) RemoveProfile(name string) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if name == "" {
		return
	}

	delete(p.Profiles, name)
	if name == p.CurProfile {
		p.CurProfile = ""
	}
	p.saveProfiles()

	broadcastMessage([]any{"thresholds", map[string]any{
		"thresholds": p.GetCurrentThresholds(),
	}})
	broadcastMessage([]any{"get_profiles", map[string]any{
		"profiles": p.GetProfileNames(),
	}})
	broadcastMessage([]any{"get_cur_profile", map[string]any{
		"cur_profile": p.CurProfile,
	}})
}

func (p *ProfileHandler) ChangeProfile(name string) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if _, ok := p.Profiles[name]; ok {
		p.CurProfile = name
		thresholds := p.GetCurrentThresholds()
		broadcastMessage([]any{"thresholds", map[string]any{
			"thresholds": thresholds,
		}})
		broadcastMessage([]any{"get_cur_profile", map[string]any{
			"cur_profile": name,
		}})
		log.Printf("Changed to profile \"%s\" with thresholds: %v", name, thresholds)
	}
}

func (p *ProfileHandler) GetProfileNames() []string {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	names := make([]string, 0) // Initialize with empty slice instead of nil
	for name := range p.Profiles {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (p *ProfileHandler) saveProfiles() {
	f, err := os.Create(p.Filename)
	if err != nil {
		log.Println("Error creating profiles file:", err)
		return
	}
	defer f.Close()

	for name, thresholds := range p.Profiles {
		if name != "" {
			line := name
			for _, t := range thresholds {
				line += " " + strconv.Itoa(t)
			}
			line += "\n"
			if _, err := f.WriteString(line); err != nil {
				log.Println("Error writing to profiles file:", err)
				return
			}
		}
	}
}

func (p *ProfileHandler) UpdateAllThresholds(values []int) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if p.CurProfile != "" {
		p.Profiles[p.CurProfile] = append([]int(nil), values...)
		p.saveProfiles()
		broadcastMessage([]any{"thresholds", map[string]any{
			"thresholds": p.GetCurrentThresholds(),
		}})
		log.Printf("Updated all thresholds: %v", values)
	}
}

func NewSerialHandler(port string, baudRate int, profile *ProfileHandler, numSensors int) *SerialHandler {
	sh := &SerialHandler{
		Port:       port,
		BaudRate:   baudRate,
		Profile:    profile,
		NumSensors: numSensors,
		writeQueue: make(chan string, 256),
	}
	sh.Open() // Open the port immediately like Python does
	return sh
}

func (s *SerialHandler) Open() bool {
	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	// Always try to close existing connection first
	if s.SerialPort != nil {
		s.SerialPort.Flush()
		s.SerialPort.Close()
		s.SerialPort = nil
		// Wait for the OS to fully release the port
		time.Sleep(time.Second * 2)
	}

	// Try to open the port with fixed delay between attempts
	maxAttempts := 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		config := &serial.Config{
			Name:        s.Port,
			Baud:        s.BaudRate,
			ReadTimeout: time.Second * 2, // Increased timeout further
		}
		port, err := serial.OpenPort(config)
		if err == nil {
			// Flush any pending data - always read even if errors to clear the buffer
			buf := make([]byte, 1024)
			for attempts := 0; attempts < 10; attempts++ {
				n, _ := port.Read(buf)
				if n == 0 {
					break
				}
			}
			port.Flush()

			// Set the port before handshake so ReadLoop can use it
			s.SerialPort = port

			// s.Profile.GetCurrentThresholds() was previously used for logging

			// Initial handshake - first request version/values, then thresholds
			_, err = port.Write([]byte("v\n"))
			if err == nil {
				reader := bufio.NewReader(port)
				response, err := reader.ReadString('\n')
				if err == nil {

					// Now request current thresholds from device
					_, err = port.Write([]byte("t\n"))
					if err == nil {
						response, err = reader.ReadString('\n')
						if err == nil {
							// Parse threshold response
							parts := strings.Fields(response)
							if len(parts) == s.NumSensors+1 && parts[0] == "t" {
								thresholds := make([]int, s.NumSensors)
								for i := 0; i < s.NumSensors; i++ {
									thresholds[i], _ = strconv.Atoi(parts[i+1])
								}
								s.Profile.UpdateAllThresholds(thresholds)
							}
							return true
						}
					}
				}
			}
			port.Close()
			s.SerialPort = nil
		}
		if attempt < maxAttempts-1 {
			// Increased delay between attempts
			time.Sleep(time.Second * 2)
		}
	}

	return false
}

// write handles all writes to the serial port in a separate goroutine
func (s *SerialHandler) write() {
	for {
		select {
		case <-shutdownSignal:
			return
		case command := <-s.writeQueue:
			if s.SerialPort != nil {
				s.Mutex.Lock()
				_, _ = s.SerialPort.Write([]byte(command))
				s.Mutex.Unlock()
			}
		}
	}
}

// trySendSerial is a helper for non-blocking send to the serial write queue.
// If the channel is full, the command is dropped and a warning is logged.
func trySendSerial(cmd string) {
	select {
	case serialHandler.writeQueue <- cmd:
	default:
		// Channel full, command dropped
	}
}

// ReadLoop implements a proper line-based read cycle with synchronized writes
func (s *SerialHandler) ReadLoop() {
	for {
		// Try to open port if needed
		if s.SerialPort == nil {
			if !s.Open() {
				time.Sleep(time.Second)
				continue
			}
		}

		// Request values
		trySendSerial("v\n")

		// Read response
		reader := bufio.NewReader(s.SerialPort)
		line, err := reader.ReadString('\n')
		if err != nil {
			log.Printf("Error reading from serial port: %v", err)
			if s.SerialPort != nil {
				s.SerialPort.Close()
				s.SerialPort = nil
			}
			continue
		}

		// Parse response
		parts := strings.Fields(line)
		if len(parts) != s.NumSensors+1 {
			continue
		}

		cmd := parts[0]
		values := make([]int, s.NumSensors)
		for i := 0; i < s.NumSensors; i++ {
			values[i], _ = strconv.Atoi(parts[i+1])
		}

		// Fix sensor ordering
		actual := make([]int, s.NumSensors)
		for i := range sensor_numbers {
			actual[i] = values[sensor_numbers[i]]
		}

		switch cmd {
		case "v":
			broadcastMessage([]any{"values", map[string]any{
				"values": actual,
			}})
			// 8ms looks nice and smooth i think
			time.Sleep(8 * time.Millisecond)
		case "t":
			curThresholds := s.Profile.GetCurrentThresholds()
			for i, val := range actual {
				if cur := curThresholds[i]; cur != val {
					s.Profile.UpdateThreshold(i, val)
				}
			}
		case "s":
			log.Printf("[SERIAL] Save thresholds command acknowledged by device")
			curThresholds := s.Profile.GetCurrentThresholds()
			broadcastMessage([]any{"thresholds_persisted", map[string]any{
				"thresholds": curThresholds,
			}})
			log.Printf("[SERIAL] Thresholds saved to device: %v", curThresholds)
		case "p":
			broadcastMessage([]any{"thresholds_persisted", map[string]any{
				"thresholds": actual,
			}})
			log.Printf("[SERIAL] Thresholds persisted to device: %v", actual)
		}
	}
}

// Optimize WebSocket writes to ensure non-blocking behavior like Python's implementation
func broadcastMessage(message []any) {
	clientsMux.Lock()
	defer clientsMux.Unlock()

	for client := range clients {
		select {
		case client.send <- message:
			// Message sent successfully to client's queue
		case <-shutdownSignal:
			return
		default:
			// If queue is full, skip this client like Python does
			log.Printf("Client queue full, skipping broadcast")
		}
	}
}

// handleWS handles websocket connections
func handleWS(w http.ResponseWriter, r *http.Request) {
	log.Printf("WebSocket connection request from %s", r.RemoteAddr)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Error upgrading to websocket from %s: %v", r.RemoteAddr, err)
		return
	}
	log.Printf("WebSocket connection established with %s", r.RemoteAddr)

	// Add to active WebSockets list
	wsLock.Lock()
	activeWebSockets = append(activeWebSockets, conn)
	wsLock.Unlock()

	client := &Client{
		conn: conn,
		send: make(chan []any, 256),
	}

	clientsMux.Lock()
	clients[client] = true
	clientsMux.Unlock()

	log.Println("Client connected")

	// Send initial state (immediately, not via goroutine)
	initialMsgs := [][]any{
		{"thresholds", map[string]any{
			"thresholds": profileHandler.GetCurrentThresholds(),
		}},
		{"get_profiles", map[string]any{
			"profiles": profileHandler.GetProfileNames(),
		}},
		{"get_cur_profile", map[string]any{
			"cur_profile": profileHandler.CurProfile,
		}},
	}

	log.Printf("Sending initial state to client: %+v", initialMsgs)
	for _, msg := range initialMsgs {
		err = conn.WriteJSON(msg)
		if err != nil {
			log.Printf("Error sending initial state message %+v: %v", msg, err)
			conn.Close()
			return
		}
	}
	log.Println("Initial state sent successfully")

	// Request current thresholds from device
	serialHandler.writeQueue <- "t\n"

	// Reader goroutine
	go func() {
		defer func() {
			wsLock.Lock()
			for i, ws := range activeWebSockets {
				if ws == conn {
					activeWebSockets = append(activeWebSockets[:i], activeWebSockets[i+1:]...)
					break
				}
			}
			wsLock.Unlock()

			conn.Close()
			clientsMux.Lock()
			delete(clients, client)
			clientsMux.Unlock()

			log.Println("Client disconnected")
		}()

		for {
			var message []any
			err := conn.ReadJSON(&message)
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("error reading websocket message: %v", err)
				} else {
					log.Printf("websocket error: %v, message: %+v", err, message)
				}
				break
			}

			if len(message) > 0 {
				action, ok := message[0].(string)
				if !ok {
					continue
				}

				switch action {
				case "update_threshold":
					if len(message) >= 3 {
						if values, ok := message[1].([]any); ok {
							if index, ok := message[2].(float64); ok {
								intValues := make([]int, len(values))
								for i, v := range values {
									if fv, ok := v.(float64); ok {
										intValues[i] = int(fv)
									}
								}
								if idx := int(index); idx >= 0 && idx < len(intValues) {
									profileHandler.UpdateThreshold(idx, intValues[idx])
									cmd := fmt.Sprintf("%d %d\n", sensor_numbers[idx], intValues[idx])
									// Non-blocking send: threshold update from client
									trySendSerial(cmd) // Prevents blocking if channel is full
								}
							}
						}
					}
				case "save_thresholds":
					log.Printf("[WS] Received save_thresholds request")
					// Non-blocking send: save thresholds command
					trySendSerial("s\n") // Prevents blocking if channel is full
					log.Printf("[WS] Sent save command to serial port")
				case "add_profile":
					if len(message) >= 3 {
						if name, ok := message[1].(string); ok {
							if thresholds, ok := message[2].([]any); ok {
								intThresholds := make([]int, len(thresholds))
								for i, t := range thresholds {
									if ft, ok := t.(float64); ok {
										intThresholds[i] = int(ft)
									}
								}
								profileHandler.AddProfile(name, intThresholds)
							}
						}
					}
				case "remove_profile":
					if len(message) >= 2 {
						if name, ok := message[1].(string); ok {
							profileHandler.RemoveProfile(name)
						}
					}
				case "change_profile":
					if len(message) >= 2 {
						if name, ok := message[1].(string); ok {
							profileHandler.ChangeProfile(name)
							thresholds := profileHandler.GetCurrentThresholds()
							for i, t := range thresholds {
								cmd := fmt.Sprintf("%d %d\n", sensor_numbers[i], t)
								// Non-blocking send: apply thresholds after profile change
								trySendSerial(cmd) // Prevents blocking if channel is full
							}
						}
					}
				case "update_all_thresholds":
					if len(message) >= 2 {
						if values, ok := message[1].([]any); ok {
							intValues := make([]int, len(values))
							for i, v := range values {
								if fv, ok := v.(float64); ok {
									intValues[i] = int(fv)
								}
							}
							profileHandler.UpdateAllThresholds(intValues)
							for i, t := range intValues {
								cmd := fmt.Sprintf("%d %d\n", sensor_numbers[i], t)
								// Non-blocking send: update all thresholds
								trySendSerial(cmd) // Prevents blocking if channel is full
							}
						}
					}
				}
			}
		}
	}()

	// Writer goroutine - handles outgoing messages
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()

		for {
			select {
			case message, ok := <-client.send:
				if !ok {
					return
				}
				// SetWriteDeadline prevents blocking
				conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
				if err := conn.WriteJSON(message); err != nil {
					if !websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						log.Printf("Error writing to websocket: %v", err)
					}
					return
				}
			case <-shutdownSignal:
				return
			case <-ticker.C:
				// Send ping to keep connection alive, like Python's keepalive
				if err := conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(100*time.Millisecond)); err != nil {
					return
				}
			}
		}
	}()
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(buildDir, "index.html"))
}

func defaultsHandler(w http.ResponseWriter, r *http.Request) {
	// Set CORS headers
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Content-Type", "application/json")

	// Handle preflight requests
	if r.Method == "OPTIONS" {
		return
	}

	// Always return an empty string rather than null.
	profiles := profileHandler.GetProfileNames()
	if profiles == nil {
		profiles = []string{}
	}

	thresholds := profileHandler.GetCurrentThresholds()
	if thresholds == nil {
		thresholds = make([]int, NUM_SENSORS)
	}

	curProfile := profileHandler.CurProfile
	if curProfile == "" {
		curProfile = ""
	}

	response := map[string]any{
		"profiles":    profiles,
		"cur_profile": curProfile,
		"thresholds":  thresholds,
	}

	log.Printf("Sending defaults response: %+v", response)
	json.NewEncoder(w).Encode(response)
}

func discoverIP() string {
	hostname, err := os.Hostname()
	if err != nil {
		return "127.0.0.1"
	}
	addrs, err := net.LookupHost(hostname)
	if err != nil || len(addrs) == 0 {
		return "127.0.0.1"
	}
	// Prefer the first non-loopback IPv4 address, else fallback to the first address.
	for _, addr := range addrs {
		ip := net.ParseIP(addr)
		if ip != nil && !ip.IsLoopback() && ip.To4() != nil {
			return addr
		}
	}
	return addrs[0]
}

func main() {
	// Define command line flags for configuration:
	// --gamepad: Specifies the serial port where the FSR (Force Sensitive Resistor) device is connected
	// --port: The network port where the web interface will be served
	// --sensors: The number of FSR sensors connected to the device
	// e.g. .\server.exe --gamepad COM4 --port 5678 --sensors 8
	gamepad := flag.String("gamepad", "/dev/ttyACM0", "Serial port to use (e.g., COM5 on Windows, /dev/ttyACM0 on Linux)")
	port := flag.String("port", "5000", "Port for the server to listen on")
	numSensors := flag.Int("sensors", NUM_SENSORS, "Number of FSR sensors (default 4)")

	// Parse command line flags
	flag.Parse()

	// Update the global sensor count if the command line argument was specified
	NUM_SENSORS = *numSensors

	// Ensure the build directory path is properly formatted for the current OS
	buildDir = filepath.Clean(buildDir)

	// Set up graceful shutdown handling for the server
	// This ensures we properly close all connections when the program exits
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Launch a goroutine to handle shutdown signals upon Ctrl+C or SIGTERM
	go func() {
		<-sigChan
		log.Println("Shutting down...")
		close(shutdownSignal)
	}()

	profileHandler = NewProfileHandler("profiles.txt", NUM_SENSORS)
	serialHandler = NewSerialHandler(*gamepad, 115200, profileHandler, NUM_SENSORS)

	// Set up the HTTP router for the web interface
	r := mux.NewRouter()

	// Match Python's routes exactly
	r.HandleFunc("/defaults", defaultsHandler).Methods("GET")
	r.HandleFunc("/ws", handleWS)

	// Create static file server
	staticFs := http.FileServer(http.Dir(buildDir))

	// SPA routes first
	r.HandleFunc("/", getIndex)
	r.HandleFunc("/plot", getIndex)

	// Then specific static file routes
	r.PathPrefix("/static/").Handler(staticFs)
	r.PathPrefix("/favicon.ico").Handler(staticFs)
	r.PathPrefix("/manifest.json").Handler(staticFs)
	r.PathPrefix("/logo").Handler(staticFs)

	// Catch-all route last
	r.PathPrefix("/").Handler(staticFs)

	// Initialize the HTTP server
	srv := &http.Server{
		Addr:    ":" + *port, // :port means listen on all interfaces (0.0.0.0)
		Handler: r,
	}

	// Start the HTTP server
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe(): %v", err)
		}
	}()

	// Show both localhost and network IP like Python would
	fmt.Printf(" * WebUI can be found at: http://%s:%s\n", discoverIP(), *port)

	// onStartup function loads profiles and starts serial communication
	onStartup()

	// Wait for shutdown signal
	<-shutdownSignal

	// Close all WebSocket connections, stop accepting new HTTP requests,
	// wait for up to 5 seconds and then close the serial port connection.
	// Note this this isn't an ideal way to release the serial port,
	// it doesn't seem perfectly reliably (once in a while the exe can't
	// access the port so it needs to be closed, but it will work on the next run).
	onShutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Fatalf("Server Shutdown Failed: %v", err)
	}
	log.Println("Server exited cleanly")
}
