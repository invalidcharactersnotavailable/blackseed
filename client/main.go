package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"sync"
	"time"

	"github.com/creack/pty" // For Unix PTY support
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Message represents data exchanged between client and server
type Message struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Shell   string `json:"shell,omitempty"`
	User    string `json:"user,omitempty"`
}

// Configuration for the client
type Config struct {
	ServerURL      string        `json:"server_url"`
	ReconnectDelay time.Duration `json:"reconnect_delay"`
	MaxRetries     int           `json:"max_retries"`
	DefaultShell   string        `json:"default_shell"`
	DefaultUser    string        `json:"default_user"`
}

var (
	// Default configuration
	config = Config{
		ServerURL:      "ws://localhost:8080/ws",
		ReconnectDelay: 5 * time.Second,
		MaxRetries:     -1, // -1 means infinite retries
		DefaultShell:   "", // Empty means use system default
		DefaultUser:    "", // Empty means current user
	}

	// Terminal process
	shellCmd   *exec.Cmd
	ptmx       *os.File
	shellMutex sync.Mutex

	// For Windows pipe handling
	winStdout io.ReadCloser
	winStderr io.ReadCloser
	winStdin  io.WriteCloser

	// Current shell and user
	currentShell string
	currentUser  string
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile) // date, time, and file info

	// Generate a unique ID for this client
	id := uuid.New().String()
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
		log.Println("Failed to get hostname:", err)
	}

	// Determine default shell if not specified
	if config.DefaultShell == "" {
		if runtime.GOOS == "windows" {
			config.DefaultShell = "cmd.exe"
		} else {
			shell := os.Getenv("SHELL")
			if shell == "" {
				shell = "/bin/sh"
			}
			config.DefaultShell = shell
		}
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

	log.Printf("Client starting with ID: %s, Hostname: %s, User: %s, Shell: %s",
		id, hostname, currentUser, currentShell)

	// Connect to server with retry logic
	retries := 0
	for config.MaxRetries < 0 || retries <= config.MaxRetries {
		if retries > 0 {
			log.Printf("Reconnecting in %v... (Attempt %d)", config.ReconnectDelay, retries)
			time.Sleep(config.ReconnectDelay)
		}

		err := connectToServer(id, hostname)
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

// connectToServer establishes a WebSocket connection to the server
func connectToServer(id, hostname string) error {
	// Connect to WebSocket server
	log.Printf("Connecting to %s", config.ServerURL)
	conn, _, err := websocket.DefaultDialer.Dial(config.ServerURL, nil)
	if err != nil {
		return fmt.Errorf("dial error: %v", err)
	}
	defer conn.Close()

	// Send initial identification
	clientInfo := map[string]interface{}{
		"uuid":     id,
		"hostname": hostname,
		"user":     currentUser,
		"shell":    currentShell,
	}

	if err := conn.WriteJSON(clientInfo); err != nil {
		return fmt.Errorf("failed to send identification: %v", err)
	}

	log.Printf("Connected to server, sent identification")

	// Start shell process
	if err := startShell(currentShell, currentUser); err != nil {
		return fmt.Errorf("failed to start shell: %v", err)
	}
	defer stopShell()

	// Create channels for communication
	done := make(chan struct{})

	// Read from WebSocket and write to shell
	go func() {
		defer close(done)
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				log.Printf("Read error from server: %v", err)
				return
			}

			var msg Message
			if err := json.Unmarshal(message, &msg); err != nil {
				log.Printf("JSON unmarshal error: %v", err)
				continue
			}

			log.Printf("Received message from server: %+v", msg)

			// Handle different message types
			switch msg.Type {
			case "command":
				shellMutex.Lock()

				if runtime.GOOS == "windows" {
					if winStdin != nil {
						log.Printf("Writing command to Windows shell: %s", msg.Content)
						_, err = winStdin.Write([]byte(msg.Content + "\r\n"))
						if err != nil {
							log.Printf("Failed to write to Windows shell: %v", err)
						}
					} else {
						log.Printf("Windows stdin is nil, cannot write command")
					}
				} else {
					if ptmx != nil {
						log.Printf("Writing command to Unix shell: %s", msg.Content)
						_, err = ptmx.Write([]byte(msg.Content + "\n"))
						if err != nil {
							log.Printf("Failed to write to Unix shell: %v", err)
						}
					} else {
						log.Printf("Unix PTY is nil, cannot write command")
					}
				}

				shellMutex.Unlock()

			case "shell_change":
				// Change shell if requested
				if msg.Shell != "" && msg.Shell != currentShell {
					log.Printf("Changing shell from %s to %s", currentShell, msg.Shell)

					// Stop current shell
					stopShell()

					// Update current shell
					currentShell = msg.Shell

					// Start new shell
					if err := startShell(currentShell, currentUser); err != nil {
						log.Printf("Failed to start new shell %s: %v", currentShell, err)

						// Send error message back to server
						errorMsg := Message{
							Type:    "error",
							Content: fmt.Sprintf("Failed to start shell %s: %v", currentShell, err),
						}
						conn.WriteJSON(errorMsg)

						// Revert to default shell
						currentShell = config.DefaultShell
						if err := startShell(currentShell, currentUser); err != nil {
							log.Printf("Failed to restart default shell: %v", err)
						}
					}

					// Send confirmation message
					conn.WriteJSON(Message{
						Type:    "output",
						Content: fmt.Sprintf("Shell changed to: %s\n$ ", currentShell),
					})
				}

			case "user_change":
				// Change user if requested
				if msg.User != "" && msg.User != currentUser {
					log.Printf("Changing user from %s to %s", currentUser, msg.User)

					// Stop current shell
					stopShell()

					// Update current user
					currentUser = msg.User

					// Start new shell with new user
					if err := startShell(currentShell, currentUser); err != nil {
						log.Printf("Failed to start shell as user %s: %v", currentUser, err)

						// Send error message back to server
						errorMsg := Message{
							Type:    "error",
							Content: fmt.Sprintf("Failed to start shell as user %s: %v", currentUser, err),
						}
						conn.WriteJSON(errorMsg)

						// Revert to previous user
						u, err := user.Current()
						if err == nil {
							currentUser = u.Username
						} else {
							currentUser = "unknown"
						}

						if err := startShell(currentShell, currentUser); err != nil {
							log.Printf("Failed to restart shell with default user: %v", err)
						}
					}

					// Send confirmation message
					conn.WriteJSON(Message{
						Type:    "output",
						Content: fmt.Sprintf("User changed to: %s\n$ ", currentUser),
					})
				}
			}
		}
	}()

	// Send initial shell prompt
	initialMsg := Message{
		Type:    "output",
		Content: fmt.Sprintf("Shell initialized: %s as user %s\n$ ", currentShell, currentUser),
	}
	if err := conn.WriteJSON(initialMsg); err != nil {
		log.Printf("Failed to send initial prompt: %v", err)
	}

	// Start reading from shell and sending output
	if runtime.GOOS == "windows" {
		go readFromPipeAndSend(conn, winStdout, "stdout")
		go readFromPipeAndSend(conn, winStderr, "stderr")
	} else {
		go readFromPTYAndSend(conn)
	}

	// Wait for done signal
	<-done

	// Attempt a graceful close
	err = conn.WriteMessage(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
	)
	if err != nil {
		return fmt.Errorf("write close error: %v", err)
	}

	return nil
}

// startShell starts a new shell process
func startShell(shellPath, username string) error {
	shellMutex.Lock()
	defer shellMutex.Unlock()

	var cmd *exec.Cmd
	var err error

	log.Printf("Starting shell: %s as user: %s", shellPath, username)

	// Determine which shell to use based on OS
	if runtime.GOOS == "windows" {
		// Windows doesn't support user switching easily in this context
		// and doesn't need PTY
		cmd = exec.Command(shellPath)

		// Windows doesn't support PTY, so we'll use standard pipes
		winStdin, err = cmd.StdinPipe()
		if err != nil {
			return fmt.Errorf("failed to get stdin pipe: %v", err)
		}

		winStdout, err = cmd.StdoutPipe()
		if err != nil {
			return fmt.Errorf("failed to get stdout pipe: %v", err)
		}

		winStderr, err = cmd.StderrPipe()
		if err != nil {
			return fmt.Errorf("failed to get stderr pipe: %v", err)
		}

		if err := cmd.Start(); err != nil {
			return fmt.Errorf("failed to start shell: %v", err)
		}

		shellCmd = cmd

	} else {
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
	}

	log.Printf("Shell started successfully")
	return nil
}

// readFromPipeAndSend reads from a pipe and sends the output to the server
func readFromPipeAndSend(conn *websocket.Conn, pipe io.ReadCloser, pipeType string) {
	buf := make([]byte, 1024)
	for {
		n, err := pipe.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading from %s: %v", pipeType, err)
			}
			break
		}

		if n > 0 {
			output := string(buf[:n])
			log.Printf("%s output: %s", pipeType, output)

			msg := Message{
				Type:    "output",
				Content: output,
			}

			if err := conn.WriteJSON(msg); err != nil {
				log.Printf("WebSocket write error: %v", err)
				break
			}
		}
	}
}

// readFromPTYAndSend reads from the PTY and sends the output to the server
func readFromPTYAndSend(conn *websocket.Conn) {
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

			msg := Message{
				Type:    "output",
				Content: output,
			}

			if err := conn.WriteJSON(msg); err != nil {
				log.Printf("WebSocket write error: %v", err)
				break
			}
		}
	}
}

// stopShell terminates the shell process
func stopShell() {
	shellMutex.Lock()
	defer shellMutex.Unlock()

	if shellCmd != nil && shellCmd.Process != nil {
		shellCmd.Process.Kill()
		shellCmd.Wait()
	}

	if runtime.GOOS == "windows" {
		if winStdin != nil {
			winStdin.Close()
			winStdin = nil
		}
		if winStdout != nil {
			winStdout.Close()
			winStdout = nil
		}
		if winStderr != nil {
			winStderr.Close()
			winStderr = nil
		}
	} else {
		if ptmx != nil {
			ptmx.Close()
			ptmx = nil
		}
	}

	shellCmd = nil
	log.Printf("Shell stopped")
}
