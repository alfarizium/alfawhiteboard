package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Message types
const (
	MessageTypeDraw  = "draw"
	MessageTypeClear = "clear"
	MessageTypeJoin  = "join"
	MessageTypeLeave = "leave"
)

// DrawPoint represents a point on the whiteboard
type DrawPoint struct {
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
	Color string  `json:"color"`
	Size  int     `json:"size"`
}

// Message represents a WebSocket message
type Message struct {
	Type      string      `json:"type"`
	UserID    string      `json:"userId"`
	Timestamp int64       `json:"timestamp"`
	Data      interface{} `json:"data,omitempty"`
}

// DrawMessage represents drawing data
type DrawMessage struct {
	Points    []DrawPoint `json:"points"`
	IsDrawing bool        `json:"isDrawing"`
	StrokeID  string      `json:"strokeId"`
}

// Client represents a connected user
type Client struct {
	ID     string
	Conn   *websocket.Conn
	Send   chan []byte
	Hub    *Hub
	UserID string
}

// Hub maintains active clients and broadcasts messages
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
	strokes    []DrawMessage // Store all drawing strokes
	mutex      sync.RWMutex
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow connections from any origin
	},
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte, 1024), // Increased buffer size
		register:   make(chan *Client),
		unregister: make(chan *Client),
		strokes:    make([]DrawMessage, 0),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
			log.Printf("Client %s connected", client.UserID)

			// Send existing strokes to new client
			h.mutex.RLock()
			for _, stroke := range h.strokes {
				message := Message{
					Type: MessageTypeDraw,
					Data: stroke,
				}
				if data, err := json.Marshal(message); err == nil {
					select {
					case client.Send <- data:
					default:
						close(client.Send)
						delete(h.clients, client)
					}
				}
			}
			h.mutex.RUnlock()

		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.Send)
				log.Printf("Client %s disconnected", client.UserID)
			}

		case message := <-h.broadcast:
			// Parse message to store strokes
			var msg Message
			if err := json.Unmarshal(message, &msg); err == nil {
				if msg.Type == MessageTypeDraw {
					if drawData, ok := msg.Data.(map[string]interface{}); ok {
						var drawMsg DrawMessage
						if data, err := json.Marshal(drawData); err == nil {
							if err := json.Unmarshal(data, &drawMsg); err == nil {
								h.mutex.Lock()
								h.strokes = append(h.strokes, drawMsg)
								h.mutex.Unlock()
							}
						}
					}
				} else if msg.Type == MessageTypeClear {
					h.mutex.Lock()
					h.strokes = make([]DrawMessage, 0)
					h.mutex.Unlock()
				}
			}

			// Broadcast to all clients
			for client := range h.clients {
				select {
				case client.Send <- message:
				default:
					close(client.Send)
					delete(h.clients, client)
				}
			}
		}
	}
}

func (c *Client) readPump() {
	defer func() {
		c.Hub.unregister <- c
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(8192) // Increased limit for drawing messages
	c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error for user %s: %v", c.UserID, err)
			}
			break
		}

		c.Conn.SetReadDeadline(time.Now().Add(60 * time.Second))

		// Add message size logging for debugging
		log.Printf("Received message from %s, size: %d bytes", c.UserID, len(message))

		// Parse and validate message
		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("Invalid message format from user %s: %v", c.UserID, err)
			continue
		}

		msg.UserID = c.UserID

		// Re-marshal with user ID
		if data, err := json.Marshal(msg); err == nil {
			select {
			case c.Hub.broadcast <- data:
			default:
				log.Printf("Broadcast channel full for user %s", c.UserID)
				close(c.Send)
				return
			}
		} else {
			log.Printf("Error marshaling message from user %s: %v", c.UserID, err)
		}
	}
}

func (c *Client) writePump() {
	ticker := time.NewTicker(54 * time.Second)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				log.Println(err)
				return
			}
		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func handleWebSocket(hub *Hub, w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}

	userID := r.Header.Get("X-User-ID")
	if userID == "" {
		userID = fmt.Sprintf("user_%d", len(hub.clients)+1)
	}

	client := &Client{
		Conn:   conn,
		Send:   make(chan []byte, 256),
		Hub:    hub,
		UserID: userID,
	}

	client.Hub.register <- client

	go client.writePump()
	go client.readPump()
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte(htmlContent))
}

const htmlContent = `<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Collaborative Whiteboard</title>
    <style>
        * {
            margin: 0;
            padding: 0;
            box-sizing: border-box;
        }

        body {
            font-family: 'Segoe UI', Tahoma, Geneva, Verdana, sans-serif;
            background: linear-gradient(135deg, #667eea 0%, #764ba2 100%);
            min-height: 100vh;
            display: flex;
            flex-direction: column;
        }

        .header {
            background: rgba(255, 255, 255, 0.1);
            backdrop-filter: blur(10px);
            padding: 1rem;
            border-bottom: 1px solid rgba(255, 255, 255, 0.2);
        }

        .toolbar {
            display: flex;
            align-items: center;
            gap: 1rem;
            flex-wrap: wrap;
            justify-content: center;
        }

        .tool-group {
            display: flex;
            align-items: center;
            gap: 0.5rem;
            background: rgba(255, 255, 255, 0.1);
            padding: 0.5rem;
            border-radius: 8px;
            backdrop-filter: blur(5px);
        }

        .tool-group label {
            color: white;
            font-weight: 500;
            font-size: 0.9rem;
        }

        .color-picker {
            width: 40px;
            height: 40px;
            border: none;
            border-radius: 50%;
            cursor: pointer;
            transition: transform 0.2s;
        }

        .color-picker:hover {
            transform: scale(1.1);
        }

        .size-slider {
            width: 100px;
            height: 6px;
            background: rgba(255, 255, 255, 0.3);
            border-radius: 3px;
            outline: none;
            cursor: pointer;
        }

        .btn {
            background: rgba(255, 255, 255, 0.2);
            border: 1px solid rgba(255, 255, 255, 0.3);
            color: white;
            padding: 0.5rem 1rem;
            border-radius: 6px;
            cursor: pointer;
            font-weight: 500;
            transition: all 0.3s;
        }

        .btn:hover {
            background: rgba(255, 255, 255, 0.3);
            transform: translateY(-2px);
        }

        .btn.danger {
            background: rgba(255, 59, 48, 0.3);
            border-color: rgba(255, 59, 48, 0.5);
        }

        .btn.danger:hover {
            background: rgba(255, 59, 48, 0.5);
        }

        .canvas-container {
            flex: 1;
            display: flex;
            justify-content: center;
            align-items: center;
            padding: 1rem;
            position: relative;
        }

        #whiteboard {
            background: white;
            border-radius: 12px;
            box-shadow: 0 20px 40px rgba(0, 0, 0, 0.1);
            cursor: crosshair;
            touch-action: none;
        }

        .status {
            position: fixed;
            top: 20px;
            right: 20px;
            background: rgba(0, 0, 0, 0.8);
            color: white;
            padding: 0.5rem 1rem;
            border-radius: 20px;
            font-size: 0.9rem;
            backdrop-filter: blur(10px);
        }

        .status.connected {
            background: rgba(52, 199, 89, 0.8);
        }

        .status.disconnected {
            background: rgba(255, 59, 48, 0.8);
        }

        .users-online {
            background: rgba(255, 255, 255, 0.1);
            backdrop-filter: blur(10px);
            padding: 0.5rem 1rem;
            border-radius: 20px;
            color: white;
            font-size: 0.9rem;
            display: flex;
            align-items: center;
            gap: 0.5rem;
        }

        .user-dot {
            width: 8px;
            height: 8px;
            background: #4CAF50;
            border-radius: 50%;
            animation: pulse 2s infinite;
        }

        @keyframes pulse {
            0% { opacity: 1; }
            50% { opacity: 0.5; }
            100% { opacity: 1; }
        }

        .size-display {
            color: white;
            font-weight: 500;
            min-width: 30px;
            text-align: center;
        }

        @media (max-width: 768px) {
            .toolbar {
                flex-direction: column;
                gap: 0.5rem;
            }
            
            .tool-group {
                flex-wrap: wrap;
                justify-content: center;
            }
            
            #whiteboard {
                width: 95vw !important;
                height: 70vh !important;
            }
        }
    </style>
</head>
<body>
    <div class="header">
        <div class="toolbar">
            <div class="tool-group">
                <label>Color:</label>
                <input type="color" id="colorPicker" class="color-picker" value="#000000">
            </div>
            
            <div class="tool-group">
                <label>Size:</label>
                <input type="range" id="sizeSlider" class="size-slider" min="1" max="20" value="3">
                <span id="sizeDisplay" class="size-display">3</span>
            </div>
            
            <button id="clearBtn" class="btn danger">Clear Board</button>
            
            <div class="users-online">
                <div class="user-dot"></div>
                <span id="userCount">1 user online</span>
            </div>
        </div>
    </div>

    <div class="canvas-container">
        <canvas id="whiteboard" width="1000" height="600"></canvas>
    </div>

    <div id="status" class="status">Connecting...</div>

    <script>
        class CollaborativeWhiteboard {
            constructor() {
                this.canvas = document.getElementById('whiteboard');
                this.ctx = this.canvas.getContext('2d');
                this.colorPicker = document.getElementById('colorPicker');
                this.sizeSlider = document.getElementById('sizeSlider');
                this.sizeDisplay = document.getElementById('sizeDisplay');
                this.clearBtn = document.getElementById('clearBtn');
                this.status = document.getElementById('status');
                this.userCount = document.getElementById('userCount');
                
                this.isDrawing = false;
                this.currentStroke = [];
                this.currentStrokeId = null;
                this.ws = null;
                this.userId = this.generateUserId();
                this.connectedUsers = new Set();
                
                this.setupEventListeners();
                this.connectWebSocket();
                this.setupCanvas();
            }
            
            generateUserId() {
                return 'user_' + Math.random().toString(36).substr(2, 9);
            }
            
            setupCanvas() {
                this.ctx.lineCap = 'round';
                this.ctx.lineJoin = 'round';
                
                // Handle high DPI displays
                const dpr = window.devicePixelRatio || 1;
                const rect = this.canvas.getBoundingClientRect();
                this.canvas.width = rect.width * dpr;
                this.canvas.height = rect.height * dpr;
                this.ctx.scale(dpr, dpr);
                this.canvas.style.width = rect.width + 'px';
                this.canvas.style.height = rect.height + 'px';
            }
            
            setupEventListeners() {
                // Drawing events
                this.canvas.addEventListener('mousedown', (e) => this.startDrawing(e));
                this.canvas.addEventListener('mousemove', (e) => this.draw(e));
                this.canvas.addEventListener('mouseup', () => this.stopDrawing());
                this.canvas.addEventListener('mouseout', () => this.stopDrawing());
                
                // Touch events for mobile
                this.canvas.addEventListener('touchstart', (e) => {
                    e.preventDefault();
                    const touch = e.touches[0];
                    const mouseEvent = new MouseEvent('mousedown', {
                        clientX: touch.clientX,
                        clientY: touch.clientY
                    });
                    this.canvas.dispatchEvent(mouseEvent);
                });
                
                this.canvas.addEventListener('touchmove', (e) => {
                    e.preventDefault();
                    const touch = e.touches[0];
                    const mouseEvent = new MouseEvent('mousemove', {
                        clientX: touch.clientX,
                        clientY: touch.clientY
                    });
                    this.canvas.dispatchEvent(mouseEvent);
                });
                
                this.canvas.addEventListener('touchend', (e) => {
                    e.preventDefault();
                    const mouseEvent = new MouseEvent('mouseup', {});
                    this.canvas.dispatchEvent(mouseEvent);
                });
                
                // UI events
                this.sizeSlider.addEventListener('input', (e) => {
                    this.sizeDisplay.textContent = e.target.value;
                });
                
                this.clearBtn.addEventListener('click', () => this.clearBoard());
                
                // Window resize
                window.addEventListener('resize', () => this.setupCanvas());
            }
            
            connectWebSocket() {
                const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
                const wsUrl = ` + "`${protocol}//${window.location.host}/ws`" + `;
                
                this.ws = new WebSocket(wsUrl);
                
                this.ws.onopen = () => {
                    this.updateStatus('Connected', 'connected');
                    console.log('Connected to whiteboard server');
                };
                
                this.ws.onmessage = (event) => {
                    try {
                        const message = JSON.parse(event.data);
                        this.handleMessage(message);
                    } catch (error) {
                        console.error('Error parsing message:', error);
                    }
                };
                
                this.ws.onclose = () => {
                    this.updateStatus('Disconnected', 'disconnected');
                    console.log('Disconnected from server');
                    // Attempt to reconnect after 3 seconds
                    setTimeout(() => this.connectWebSocket(), 3000);
                };
                
                this.ws.onerror = (error) => {
                    console.error('WebSocket error:', error);
                    this.updateStatus('Connection Error', 'disconnected');
                };
            }
            
            handleMessage(message) {
                switch (message.type) {
                    case 'draw':
                        if (message.userId !== this.userId) {
                            this.drawStroke(message.data);
                        }
                        break;
                    case 'clear':
                        if (message.userId !== this.userId) {
                            this.ctx.clearRect(0, 0, this.canvas.width, this.canvas.height);
                        }
                        break;
                    case 'join':
                        this.connectedUsers.add(message.userId);
                        this.updateUserCount();
                        break;
                    case 'leave':
                        this.connectedUsers.delete(message.userId);
                        this.updateUserCount();
                        break;
                }
            }
            
            updateStatus(text, className) {
                this.status.textContent = text;
                this.status.className = ` + "`status ${className}`" + `;
            }
            
            updateUserCount() {
                const count = this.connectedUsers.size + 1; // +1 for current user
                this.userCount.textContent = ` + "`${count} user${count === 1 ? '' : 's'} online`" + `;
            }
            
            getMousePos(e) {
                const rect = this.canvas.getBoundingClientRect();
                return {
                    x: e.clientX - rect.left,
                    y: e.clientY - rect.top
                };
            }
            
            startDrawing(e) {
                this.isDrawing = true;
                this.currentStroke = [];
                this.currentStrokeId = this.generateStrokeId();
                
                const pos = this.getMousePos(e);
                const point = {
                    x: pos.x,
                    y: pos.y,
                    color: this.colorPicker.value,
                    size: parseInt(this.sizeSlider.value)
                };
                
                this.currentStroke.push(point);
                this.drawPoint(point);
            }
            
            draw(e) {
                if (!this.isDrawing) return;
                
                const pos = this.getMousePos(e);
                const point = {
                    x: Math.round(pos.x),
                    y: Math.round(pos.y),
                    color: this.colorPicker.value,
                    size: parseInt(this.sizeSlider.value)
                };
                
                this.currentStroke.push(point);
                this.drawLine(this.currentStroke[this.currentStroke.length - 2], point);
                
                // Throttle sending - only send every 3rd point or when stroke ends
                if (this.currentStroke.length % 3 === 0) {
                    this.sendDrawData();
                }
            }
            
            stopDrawing() {
                if (!this.isDrawing) return;
                this.isDrawing = false;
                
                // Send final stroke data
                this.sendDrawData();
                this.currentStroke = [];
                this.currentStrokeId = null;
            }
            
            drawPoint(point) {
                this.ctx.globalCompositeOperation = 'source-over';
                this.ctx.fillStyle = point.color;
                this.ctx.beginPath();
                this.ctx.arc(point.x, point.y, point.size / 2, 0, Math.PI * 2);
                this.ctx.fill();
            }
            
            drawLine(point1, point2) {
                this.ctx.globalCompositeOperation = 'source-over';
                this.ctx.strokeStyle = point2.color;
                this.ctx.lineWidth = point2.size;
                this.ctx.beginPath();
                this.ctx.moveTo(point1.x, point1.y);
                this.ctx.lineTo(point2.x, point2.y);
                this.ctx.stroke();
            }
            
            drawStroke(strokeData) {
                const points = strokeData.points;
                if (points.length === 0) return;
                
                if (points.length === 1) {
                    this.drawPoint(points[0]);
                } else {
                    for (let i = 1; i < points.length; i++) {
                        this.drawLine(points[i - 1], points[i]);
                    }
                }
            }
            
            generateStrokeId() {
                return Date.now() + '_' + Math.random().toString(36).substr(2, 9);
            }
            
            sendDrawData() {
                if (this.ws && this.ws.readyState === WebSocket.OPEN && this.currentStroke.length > 0) {
                    try {
                        const message = {
                            type: 'draw',
                            timestamp: Date.now(),
                            data: {
                                points: [...this.currentStroke],
                                isDrawing: this.isDrawing,
                                strokeId: this.currentStrokeId
                            }
                        };
                        
                        const messageStr = JSON.stringify(message);
                        console.log('Sending message, size:', messageStr.length);
                        this.ws.send(messageStr);
                    } catch (error) {
                        console.error('Error sending draw data:', error);
                        this.updateStatus('Send Error', 'disconnected');
                    }
                }
            }
            
            clearBoard() {
                this.ctx.clearRect(0, 0, this.canvas.width, this.canvas.height);
                
                if (this.ws && this.ws.readyState === WebSocket.OPEN) {
                    const message = {
                        type: 'clear',
                        timestamp: Date.now()
                    };
                    
                    this.ws.send(JSON.stringify(message));
                }
            }
        }
        
        // Initialize the whiteboard when the page loads
        document.addEventListener('DOMContentLoaded', () => {
            new CollaborativeWhiteboard();
        });
    </script>
</body>
</html>` + "`"

func main() {
	hub := newHub()
	go hub.run()

	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		handleWebSocket(hub, w, r)
	})

	// Serve static files
	fs := http.FileServer(http.Dir("static/"))
	http.Handle("/static/", http.StripPrefix("/static/", fs))

	fmt.Println("Collaborative Whiteboard server starting on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}
