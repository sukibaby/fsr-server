// TODO
//
// The previous version was working with the following notes
//
//  - the serial port should be closed properly on shutdown.
//  -  thresholds need to be saved properly.  currently they are saved one at a time.
//  -  make sure the react app and server are in sync with the actual thresholds being used.
//     seems like the react app is always using the set of thresholds from when the server
//     started, not the current ones.
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

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
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

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

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
	NoSerial   bool
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
	profileHandler.LoadProfiles()

	// Ensure we have at least an empty profile
	if len(profileHandler.GetProfileNames()) == 0 {
		emptyProfile := make([]int, profileHandler.NumSensors)
		profileHandler.AddProfile("Default", emptyProfile)
	}

	go serialHandler.ReadLoop()
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

func NewSerialHandler(port string, baudRate int, profile *ProfileHandler, numSensors int, noSerial bool) *SerialHandler {
	return &SerialHandler{
		Port:       port,
		BaudRate:   baudRate,
		Profile:    profile,
		NoSerial:   noSerial,
		NumSensors: numSensors,
		writeQueue: make(chan string, 256),
	}
}

func (s *SerialHandler) Open() bool {
	if s.NoSerial {
		return true
	}

	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	// Always try to close existing connection first
	if s.SerialPort != nil {
		s.SerialPort.Close()
		s.SerialPort = nil
		// Wait for the OS to fully release the port
		time.Sleep(500 * time.Millisecond)
	}

	// Try to open the port with fixed delay between attempts
	maxAttempts := 5
	for attempt := 0; attempt < maxAttempts; attempt++ {
		config := &serial.Config{
			Name:        s.Port,
			Baud:        s.BaudRate,
			ReadTimeout: time.Second, // Increased timeout
		}
		port, err := serial.OpenPort(config)
		if err == nil {
			s.SerialPort = port

			// Flush any pending data
			buf := make([]byte, 1024)
			for {
				n, err := port.Read(buf)
				if err != nil || n == 0 {
					break
				}
			}

			log.Printf("[SERIAL] Device detected on %s", s.Port)
			thresholds := s.Profile.GetCurrentThresholds()
			log.Printf("[SERIAL] Current thresholds: %v", thresholds)

			// Initial handshake - send a command and verify response
			_, err = port.Write([]byte("v\n"))
			if err == nil {
				reader := bufio.NewReader(port)
				_, err = reader.ReadString('\n')
				if err == nil {
					return true
				}
			}

			log.Printf("Port opened but handshake failed, retrying...")
			port.Close()
		}

		log.Printf("Attempt %d: Error opening serial port: %v", attempt+1, err)
		if attempt < maxAttempts-1 {
			// Increased delay between attempts
			time.Sleep(time.Second)
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
			if s.NoSerial {
				if command[0] == 't' {
					broadcastMessage([]any{"thresholds",
						map[string]any{"thresholds": s.Profile.GetCurrentThresholds()}})
					log.Println("Thresholds are:", s.Profile.GetCurrentThresholds())
				} else {
					if len(command) > 2 {
						parts := strings.Fields(command)
						if len(parts) == 2 {
							sensor, _ := strconv.Atoi(parts[0])
							threshold, _ := strconv.Atoi(parts[1])
							for i := range sensor_numbers {
								if i == sensor {
									s.Profile.UpdateThreshold(i, threshold)
								}
							}
						}
					}
				}
				continue
			}

			if s.SerialPort == nil {
				time.Sleep(time.Second)
				continue
			}

			s.Mutex.Lock()
			_, err := s.SerialPort.Write([]byte(command))
			s.Mutex.Unlock()

			if err != nil {
				log.Printf("Error writing to serial port: %v", err)
				s.SerialPort = nil
			}
		}
	}
}

// ReadLoop implements a proper line-based read cycle with synchronized writes
func (s *SerialHandler) ReadLoop() {
	lastValues := make([]int, s.NumSensors)

	// Start the write goroutine
	go s.write()

	// Start periodic threshold checks
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if s.SerialPort != nil && !s.NoSerial {
					s.writeQueue <- "t\n"
				}
			case <-shutdownSignal:
				return
			}
		}
	}()

	for {
		if s.NoSerial {
			// Simulate sensor values with normal distribution for more realistic changes
			values := make([]int, s.NumSensors)
			for i := range values {
				offset := int(rand.NormFloat64() * float64(s.NumSensors+1))
				newVal := lastValues[i] + offset
				values[i] = max(0, min(newVal, 1023))
				lastValues[i] = values[i]
			}
			broadcastMessage([]any{"values", map[string]any{"values": values}})
			time.Sleep(10 * time.Millisecond)
			continue
		}

		for {
			if s.NoSerial {
				// Simulate sensor values with normal distribution for more realistic changes
				values := make([]int, s.NumSensors)
				for i := range values {
					offset := int(rand.NormFloat64() * float64(s.NumSensors+1))
					newVal := lastValues[i] + offset
					values[i] = max(0, min(newVal, 1023))
					lastValues[i] = values[i]
				}
				broadcastMessage([]any{"values", map[string]any{"values": values}})
				time.Sleep(10 * time.Millisecond)
				continue
			}

			if s.SerialPort == nil {
				if !s.Open() {
					time.Sleep(time.Second)
					continue
				}
				// Apply current thresholds when reconnecting
				thresholds := s.Profile.GetCurrentThresholds()
				for i, t := range thresholds {
					cmd := fmt.Sprintf("%d %d\n", sensor_numbers[i], t)
					s.writeQueue <- cmd
				}
				continue
			}

			// Poll: write 'v\n' directly, then read response
			_, err := s.SerialPort.Write([]byte("v\n"))
			if err != nil {
				log.Printf("Error writing to serial port: %v, attempting to reconnect", err)
				s.SerialPort = nil
				time.Sleep(time.Second)
				continue
			}
			reader := bufio.NewReader(s.SerialPort)
			line, err := reader.ReadString('\n')
			if err != nil {
				log.Printf("Error reading from serial port: %v, attempting to reconnect", err)
				s.SerialPort = nil
				time.Sleep(time.Second)
				continue
			}

			// Trim any whitespace and split into fields
			parts := strings.Fields(line)
			if len(parts) != s.NumSensors+1 {
				// Invalid response, immediately try again
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
				time.Sleep(10 * time.Millisecond)
			case "t":
				curThresholds := s.Profile.GetCurrentThresholds()
				for i, val := range actual {
					if cur := curThresholds[i]; cur != val {
						s.Profile.UpdateThreshold(i, val)
					}
				}
			case "p":
				broadcastMessage([]any{"thresholds_persisted", map[string]any{
					"thresholds": actual,
				}})
				log.Printf("Saved thresholds to device: %v", s.Profile.GetCurrentThresholds())
			}

			time.Sleep(time.Millisecond)
		}
	}
}

// Optimize WebSocket writes to ensure non-blocking behavior
func broadcastMessage(message []any) {
	clientsMux.Lock()
	for client := range clients {
		select {
		case client.send <- message:
			// Message sent successfully
		case <-shutdownSignal:
			clientsMux.Unlock()
			return
		default:
			// If we can't send immediately, don't block
			go func(c *Client, msg []any) {
				select {
				case c.send <- msg:
					// Sent after brief wait
				case <-time.After(10 * time.Millisecond):
					// If still can't send after timeout, close connection
					close(c.send)
					clientsMux.Lock()
					delete(clients, c)
					clientsMux.Unlock()
				}
			}(client, message)
		}
	}
	clientsMux.Unlock()
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
									serialHandler.writeQueue <- cmd
								}
							}
						}
					}
				case "save_thresholds":
					serialHandler.writeQueue <- "s\n"
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
								serialHandler.writeQueue <- cmd
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
								serialHandler.writeQueue <- cmd
							}
						}
					}
				}
			}
		}
	}()

	go func() {
		for message := range client.send {
			select {
			case <-shutdownSignal:
				return
			default:
				// SetWriteDeadline prevents blocking
				conn.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
				if err := conn.WriteJSON(message); err != nil {
					if !websocket.IsCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
						log.Printf("Error writing to websocket: %v", err)
					}
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
	w.Header().Set("Content-Type", "application/json")

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
	ip := "127.0.0.1"

	ifaces, err := net.Interfaces()
	if err != nil {
		return ip
	}

	for _, iface := range ifaces {
		// Skip loopback and down interfaces
		if iface.Flags&net.FlagLoopback != 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}

		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			switch v := addr.(type) {
			case *net.IPNet:
				if v.IP.To4() != nil && !v.IP.IsLoopback() {
					return v.IP.String()
				}
			}
		}
	}
	return ip
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
	serialHandler = NewSerialHandler(*gamepad, 115200, profileHandler, NUM_SENSORS, false)

	// Set up the HTTP router for the web interface
	r := mux.NewRouter()

	// Configure the main API endpoints
	// /defaults - Returns current system state (profiles, thresholds)
	// /ws - WebSocket endpoint for real-time updates
	r.HandleFunc("/defaults", defaultsHandler).Methods("GET")
	r.HandleFunc("/ws", handleWS)
	apiRouter := r.PathPrefix("/api").Subrouter()
	apiRouter.HandleFunc("/ws", handleWS)
	apiRouter.HandleFunc("/defaults", defaultsHandler).Methods("GET")

	// Handlers for static files
	staticFs := http.FileServer(http.Dir(buildDir))
	r.PathPrefix("/static/").Handler(staticFs)
	r.PathPrefix("/favicon.ico").Handler(staticFs)
	r.PathPrefix("/manifest.json").Handler(staticFs)
	r.PathPrefix("/logo").Handler(staticFs)

	// SPA routes all return the main index.html
	r.HandleFunc("/", getIndex)
	r.HandleFunc("/plot", getIndex)
	r.PathPrefix("/").HandlerFunc(getIndex) // catch-all for other client routes

	// Initialize the HTTP server
	srv := &http.Server{
		Addr:    ":" + *port,
		Handler: r,
	}

	// onStartup function will load saved profiles from disk and start the serial communication loop
	onStartup()

	// Get the server's IPv4 IP address for displaying the access URL
	ipToShow := discoverIP()

	// Start the HTTP server in a separate goroutine
	go func() {
		fmt.Printf(" * WebUI can be found at: http://%s:%s\n", ipToShow, *port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ListenAndServe(): %v", err)
		}
	}()

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
