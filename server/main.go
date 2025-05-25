package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// Client represents a connected remote system
type Client struct {
	ID       string
	Hostname string
	User     string
	Shell    string
	Conn     *websocket.Conn
	mu       sync.Mutex
	// Add operator connections to forward output
	operators map[string]*websocket.Conn
}

// ClientManager handles all connected clients
type ClientManager struct {
	clients    map[string]*Client
	register   chan *Client
	unregister chan *Client
	mu         sync.RWMutex
}

// Message represents data exchanged between client and server
type Message struct {
	Type    string `json:"type"`
	Content string `json:"content"`
	Shell   string `json:"shell,omitempty"`
	User    string `json:"user,omitempty"`
	Target  string `json:"target,omitempty"`
}

var (
	manager = ClientManager{
		clients:    make(map[string]*Client),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true // Allow all connections in this example
		},
	}
)

func main() {
	// Start the client manager
	go manager.run()

	// Set up HTTP routes
	http.HandleFunc("/", handleHome)
	http.HandleFunc("/ws", handleConnections)
	http.HandleFunc("/clients", handleListClients)
	http.HandleFunc("/connect", handleClientConnect)

	// Start the server
	port := 8080
	log.Printf("Starting server on port %d...", port)
	if err := http.ListenAndServe(fmt.Sprintf(":%d", port), nil); err != nil {
		log.Fatal("ListenAndServe error:", err)
	}
}

// run manages client connections
func (manager *ClientManager) run() {
	for {
		select {
		case client := <-manager.register:
			manager.mu.Lock()
			manager.clients[client.ID] = client
			manager.mu.Unlock()
			log.Printf("Client connected: %s (%s) - User: %s, Shell: %s",
				client.ID, client.Hostname, client.User, client.Shell)

		case client := <-manager.unregister:
			manager.mu.Lock()
			if _, ok := manager.clients[client.ID]; ok {
				delete(manager.clients, client.ID)
				client.Conn.Close()
			}
			manager.mu.Unlock()
			log.Printf("Client disconnected: %s", client.ID)
		}
	}
}

// handleHome serves the operator interface
func handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}

	html := `
<!DOCTYPE html>
<html>
<head>
    <title>Remote Terminal Control</title>
    <style>
        body {
            font-family: 'Courier New', monospace;
            background-color: #1e1e1e;
            color: #f0f0f0;
            margin: 0;
            padding: 0;
            display: flex;
            height: 100vh;
        }
        #sidebar {
            width: 250px;
            background-color: #252526;
            padding: 10px;
            overflow-y: auto;
            border-right: 1px solid #3c3c3c;
        }
        #main {
            flex: 1;
            display: flex;
            flex-direction: column;
            padding: 10px;
        }
        #terminal {
            flex: 1;
            background-color: #0c0c0c;
            border: 1px solid #3c3c3c;
            padding: 10px;
            overflow-y: auto;
            white-space: pre-wrap;
            font-family: 'Courier New', monospace;
            color: #cccccc;
            margin-bottom: 10px;
        }
        #input {
            display: flex;
            margin-bottom: 10px;
        }
        #command {
            flex: 1;
            background-color: #1e1e1e;
            color: #f0f0f0;
            border: 1px solid #3c3c3c;
            padding: 8px;
            font-family: 'Courier New', monospace;
        }
        button {
            background-color: #0e639c;
            color: white;
            border: none;
            padding: 8px 16px;
            margin-left: 5px;
            cursor: pointer;
        }
        button:hover {
            background-color: #1177bb;
        }
        .client {
            padding: 8px;
            margin-bottom: 5px;
            background-color: #333333;
            border-radius: 3px;
            cursor: pointer;
        }
        .client:hover {
            background-color: #3c3c3c;
        }
        .client.active {
            background-color: #0e639c;
        }
        .hostname {
            font-weight: bold;
        }
        .uuid {
            font-size: 0.8em;
            color: #999999;
        }
        .user-shell {
            font-size: 0.8em;
            color: #bbbbbb;
            margin-top: 3px;
        }
        h2 {
            margin-top: 0;
            border-bottom: 1px solid #3c3c3c;
            padding-bottom: 10px;
        }
        .status {
            color: #999;
            font-style: italic;
            margin-top: 10px;
        }
        .controls {
            margin-top: 10px;
            padding-top: 10px;
            border-top: 1px solid #3c3c3c;
        }
        .control-group {
            margin-bottom: 10px;
        }
        .control-group label {
            display: block;
            margin-bottom: 5px;
            color: #bbbbbb;
        }
        .control-group input {
            width: 100%;
            background-color: #1e1e1e;
            color: #f0f0f0;
            border: 1px solid #3c3c3c;
            padding: 5px;
            font-family: 'Courier New', monospace;
        }
    </style>
</head>
<body>
    <div id="sidebar">
        <h2>Connected Clients</h2>
        <div id="clients"></div>
    </div>
    <div id="main">
        <div id="terminal"></div>
        <div id="input">
            <input type="text" id="command" placeholder="Enter command..." disabled />
            <button id="send" disabled>Send</button>
        </div>
        <div class="controls" id="controls" style="display: none;">
            <div class="control-group">
                <label for="shell-path">Custom Shell Path:</label>
                <div style="display: flex;">
                    <input type="text" id="shell-path" placeholder="/bin/bash" />
                    <button id="change-shell">Change</button>
                </div>
            </div>
            <div class="control-group">
                <label for="user-name">Run As User:</label>
                <div style="display: flex;">
                    <input type="text" id="user-name" placeholder="username" />
                    <button id="change-user">Change</button>
                </div>
            </div>
        </div>
        <div class="status" id="status">Not connected to any client</div>
    </div>

    <script>
        let activeClient = null;
        let socket = null;
        
        // Load clients on page load
        window.onload = function() {
            fetchClients();
            setInterval(fetchClients, 5000); // Refresh client list every 5 seconds
            
            // Add event listeners
            document.getElementById('send').addEventListener('click', function() {
                sendCommand();
            });
            
            document.getElementById('command').addEventListener('keypress', function(e) {
                if (e.key === 'Enter') {
                    sendCommand();
                }
            });
            
            document.getElementById('change-shell').addEventListener('click', function() {
                changeShell();
            });
            
            document.getElementById('change-user').addEventListener('click', function() {
                changeUser();
            });
        };
        
        // Fetch available clients
        function fetchClients() {
            fetch('/clients')
                .then(response => response.json())
                .then(data => {
                    const clientsDiv = document.getElementById('clients');
                    
                    // Save current active client ID
                    const activeClientId = activeClient;
                    
                    // Clear current list
                    clientsDiv.innerHTML = '';
                    
                    if (data.length === 0) {
                        clientsDiv.innerHTML = '<div class="status">No clients connected</div>';
                        return;
                    }
                    
                    // Add each client to the list
                    data.forEach(client => {
                        const clientDiv = document.createElement('div');
                        clientDiv.className = 'client';
                        if (activeClientId === client.id) {
                            clientDiv.className += ' active';
                        }
                        
                        clientDiv.innerHTML = '<div class="hostname">' + client.hostname + '</div>' +
                            '<div class="uuid">' + client.id + '</div>' +
                            '<div class="user-shell">User: ' + client.user + ' | Shell: ' + client.shell + '</div>';
                        
                        clientDiv.onclick = function() {
                            connectToClient(client.id);
                        };
                        
                        clientsDiv.appendChild(clientDiv);
                    });
                })
                .catch(error => console.error('Error fetching clients:', error));
        }
        
        // Connect to a specific client
        function connectToClient(clientId) {
            // Close existing connection if any
            if (socket) {
                socket.close();
            }
            
            activeClient = clientId;
            
            // Update UI
            document.querySelectorAll('.client').forEach(el => {
                el.classList.remove('active');
                if (el.querySelector('.uuid').textContent === clientId) {
                    el.classList.add('active');
                }
            });
            
            // Enable input
            document.getElementById('command').disabled = false;
            document.getElementById('send').disabled = false;
            document.getElementById('controls').style.display = 'block';
            
            // Clear terminal
            document.getElementById('terminal').textContent = '';
            document.getElementById('status').textContent = 'Connected to client: ' + clientId;
            
            // Connect WebSocket
            const protocol = window.location.protocol === 'https:' ? 'wss://' : 'ws://';
            socket = new WebSocket(protocol + window.location.host + '/connect?id=' + clientId);
            
            socket.onopen = function(e) {
                appendToTerminal('=== Connection established ===\n');
            };
            
            socket.onmessage = function(event) {
                try {
                    const msg = JSON.parse(event.data);
                    if (msg.type === 'output') {
                        appendToTerminal(msg.content);
                    } else if (msg.type === 'error') {
                        appendToTerminal('ERROR: ' + msg.content + '\n');
                    }
                } catch (e) {
                    console.error('Error parsing message:', e);
                    appendToTerminal('Error: ' + e.message + '\n');
                }
            };
            
            socket.onclose = function(event) {
                if (event.wasClean) {
                    appendToTerminal('\n=== Connection closed cleanly, code=' + event.code + ' reason=' + event.reason + ' ===\n');
                } else {
                    appendToTerminal('\n=== Connection died ===\n');
                }
                
                document.getElementById('command').disabled = true;
                document.getElementById('send').disabled = true;
                document.getElementById('controls').style.display = 'none';
                document.getElementById('status').textContent = 'Disconnected';
            };
            
            socket.onerror = function(error) {
                appendToTerminal('\n=== Error: ' + error.message + ' ===\n');
                document.getElementById('status').textContent = 'Error: Connection failed';
            };
        }
        
        // Append text to terminal with auto-scroll
        function appendToTerminal(text) {
            const terminal = document.getElementById('terminal');
            terminal.textContent += text;
            terminal.scrollTop = terminal.scrollHeight;
        }
        
        // Send command to server
        function sendCommand() {
            const commandInput = document.getElementById('command');
            const command = commandInput.value;
            
            if (command && socket && socket.readyState === WebSocket.OPEN) {
                const message = {
                    type: 'command',
                    content: command
                };
                
                socket.send(JSON.stringify(message));
                appendToTerminal('$ ' + command + '\n');
                commandInput.value = '';
            }
        }
        
        // Change shell
        function changeShell() {
            const shellInput = document.getElementById('shell-path');
            const shell = shellInput.value.trim();
            
            if (shell && socket && socket.readyState === WebSocket.OPEN) {
                const message = {
                    type: 'shell_change',
                    shell: shell
                };
                
                socket.send(JSON.stringify(message));
                appendToTerminal('\n=== Changing shell to: ' + shell + ' ===\n');
                shellInput.value = '';
            }
        }
        
        // Change user
        function changeUser() {
            const userInput = document.getElementById('user-name');
            const user = userInput.value.trim();
            
            if (user && socket && socket.readyState === WebSocket.OPEN) {
                const message = {
                    type: 'user_change',
                    user: user
                };
                
                socket.send(JSON.stringify(message));
                appendToTerminal('\n=== Changing user to: ' + user + ' ===\n');
                userInput.value = '';
            }
        }
    </script>
</body>
</html>
`
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, html)
}

// handleConnections handles incoming WebSocket connections from clients
func handleConnections(w http.ResponseWriter, r *http.Request) {
	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	// Read initial identification message
	_, msg, err := conn.ReadMessage()
	if err != nil {
		log.Println("Read error:", err)
		conn.Close()
		return
	}

	// Parse client identification
	var clientData map[string]interface{}
	if err := json.Unmarshal(msg, &clientData); err != nil {
		log.Println("JSON unmarshal error:", err)
		conn.Close()
		return
	}

	clientID, _ := clientData["uuid"].(string)
	hostname, _ := clientData["hostname"].(string)
	user, _ := clientData["user"].(string)
	shell, _ := clientData["shell"].(string)

	// Create new client
	client := &Client{
		ID:        clientID,
		Hostname:  hostname,
		User:      user,
		Shell:     shell,
		Conn:      conn,
		operators: make(map[string]*websocket.Conn),
	}

	// Register client
	manager.register <- client

	// Handle incoming messages
	go handleClient(client)
}

// handleClient processes messages from a connected client
func handleClient(client *Client) {
	defer func() {
		manager.unregister <- client
	}()

	for {
		_, message, err := client.Conn.ReadMessage()
		if err != nil {
			log.Printf("Read error from client %s: %v", client.ID, err)
			break
		}

		// Process message
		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("JSON unmarshal error from client %s: %v", client.ID, err)
			continue
		}

		log.Printf("Received message from %s: %s", client.ID, string(message))

		// If this is an output message, forward it to all connected operators
		if msg.Type == "output" {
			client.mu.Lock()
			for sessionID, operatorConn := range client.operators {
				err := operatorConn.WriteJSON(msg)
				if err != nil {
					log.Printf("Error forwarding output to operator session %s: %v", sessionID, err)
					delete(client.operators, sessionID)
				}
			}
			client.mu.Unlock()
		}
	}
}

// handleListClients returns a list of connected clients
func handleListClients(w http.ResponseWriter, r *http.Request) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()

	clients := make([]map[string]string, 0, len(manager.clients))
	for _, client := range manager.clients {
		clients = append(clients, map[string]string{
			"id":       client.ID,
			"hostname": client.Hostname,
			"user":     client.User,
			"shell":    client.Shell,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(clients)
}

// handleClientConnect handles WebSocket connections from the operator to a specific client
func handleClientConnect(w http.ResponseWriter, r *http.Request) {
	clientID := r.URL.Query().Get("id")
	if clientID == "" {
		http.Error(w, "Missing client ID", http.StatusBadRequest)
		return
	}

	// Check if client exists
	manager.mu.RLock()
	targetClient, exists := manager.clients[clientID]
	manager.mu.RUnlock()

	if !exists {
		http.Error(w, "Client not found", http.StatusNotFound)
		return
	}

	// Upgrade HTTP connection to WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	// Create a unique session ID for this operator connection
	sessionID := uuid.New().String()
	log.Printf("Operator connected to client %s with session %s", clientID, sessionID)

	// Register this operator connection with the client
	targetClient.mu.Lock()
	targetClient.operators[sessionID] = conn
	targetClient.mu.Unlock()

	// Send initial welcome message
	welcomeMsg := Message{
		Type: "output",
		Content: fmt.Sprintf("Connected to %s (%s)\nUser: %s | Shell: %s\n$ ",
			targetClient.Hostname, targetClient.ID, targetClient.User, targetClient.Shell),
	}
	conn.WriteJSON(welcomeMsg)

	// Handle operator commands
	go handleOperatorCommands(conn, targetClient, sessionID)
}

// handleOperatorCommands processes commands from the operator and forwards them to the client
func handleOperatorCommands(conn *websocket.Conn, targetClient *Client, sessionID string) {
	defer func() {
		conn.Close()
		targetClient.mu.Lock()
		delete(targetClient.operators, sessionID)
		targetClient.mu.Unlock()
		log.Printf("Operator session %s disconnected", sessionID)
	}()

	for {
		// Read message from operator
		_, msg, err := conn.ReadMessage()
		if err != nil {
			log.Printf("Operator session %s disconnected: %v", sessionID, err)
			break
		}

		// Parse message
		var message Message
		if err := json.Unmarshal(msg, &message); err != nil {
			log.Printf("Invalid message format from operator session %s: %v", sessionID, err)
			continue
		}

		log.Printf("Received message from operator session %s: %+v", sessionID, message)

		// Forward message to target client based on type
		switch message.Type {
		case "command":
			log.Printf("Forwarding command to client %s: %s", targetClient.ID, message.Content)
			targetClient.mu.Lock()
			err := targetClient.Conn.WriteJSON(message)
			targetClient.mu.Unlock()

			if err != nil {
				log.Printf("Error sending command to client %s: %v", targetClient.ID, err)
				errorMsg := Message{
					Type:    "error",
					Content: "Failed to send command to client: " + err.Error(),
				}
				conn.WriteJSON(errorMsg)
			}

		case "shell_change":
			log.Printf("Forwarding shell change request to client %s: %s", targetClient.ID, message.Shell)
			targetClient.mu.Lock()
			err := targetClient.Conn.WriteJSON(message)
			targetClient.mu.Unlock()

			if err != nil {
				log.Printf("Error sending shell change to client %s: %v", targetClient.ID, err)
				errorMsg := Message{
					Type:    "error",
					Content: "Failed to send shell change request: " + err.Error(),
				}
				conn.WriteJSON(errorMsg)
			} else {
				// Update client shell in our records
				targetClient.Shell = message.Shell
			}

		case "user_change":
			log.Printf("Forwarding user change request to client %s: %s", targetClient.ID, message.User)
			targetClient.mu.Lock()
			err := targetClient.Conn.WriteJSON(message)
			targetClient.mu.Unlock()

			if err != nil {
				log.Printf("Error sending user change to client %s: %v", targetClient.ID, err)
				errorMsg := Message{
					Type:    "error",
					Content: "Failed to send user change request: " + err.Error(),
				}
				conn.WriteJSON(errorMsg)
			} else {
				// Update client user in our records
				targetClient.User = message.User
			}
		}
	}
}
