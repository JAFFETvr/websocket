package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Estructura para gestionar clientes conectados
type Hub struct {
	clients    map[*websocket.Conn]bool // Clientes registrados (viewers)
	broadcast  chan []byte              // Canal para enviar mensajes (frames) a los viewers
	register   chan *websocket.Conn     // Canal para registrar viewers
	unregister chan *websocket.Conn     // Canal para desregistrar viewers
	camConn    *websocket.Conn          // Referencia a la conexión de la cámara (asumimos una sola cámara)
	mu         sync.Mutex               // Mutex para proteger el acceso a camConn y clients
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []byte, 5), // Buffer para no bloquear la cámara si los viewers van lentos
		register:   make(chan *websocket.Conn),
		unregister: make(chan *websocket.Conn),
		camConn:    nil,
	}
}

// Goroutine principal del Hub para gestionar clientes y mensajes
func (h *Hub) run() {
	for {
		select {
		// Un nuevo viewer se conecta
		case conn := <-h.register:
			h.mu.Lock()
			h.clients[conn] = true
			log.Println("Viewer registered:", conn.RemoteAddr())
			h.mu.Unlock()

		// Un viewer se desconecta
		case conn := <-h.unregister:
			h.mu.Lock()
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				conn.Close() // Asegurarse de cerrar la conexión
				log.Println("Viewer unregistered:", conn.RemoteAddr())
			}
			h.mu.Unlock()

		// Mensaje recibido (asumimos que es de la cámara y lo enviamos a los viewers)
		case message := <-h.broadcast:
			h.mu.Lock()
			// Enviar a todos los viewers registrados
			for conn := range h.clients {
				// Usar goroutine para no bloquear el broadcast si un cliente es lento
				go func(c *websocket.Conn, msg []byte) {
					h.mu.Lock() // Necesario bloquear aquí también por si se desregistra mientras enviamos
					_, clientExists := h.clients[c]
					h.mu.Unlock()

					if clientExists { // Solo enviar si todavía está registrado
						// Usamos WriteMessage aquí, podría ser mejor usar NextWriter para streaming
						err := c.WriteMessage(websocket.BinaryMessage, msg)
						if err != nil {
							log.Printf("Error sending to viewer %v: %v. Unregistering.", c.RemoteAddr(), err)
							// Si hay error al escribir, asumir que el cliente se desconectó
							h.unregister <- c 
						}
					}
				}(conn, message)
			}
			h.mu.Unlock()
		}
	}
}

// Lee mensajes de una conexión específica (cámara o viewer)
func (h *Hub) reader(conn *websocket.Conn) {
	isCam := false // Flag para saber si esta es la cámara
	defer func() {
		h.mu.Lock()
		// Si era la cámara, limpiamos la referencia
		if isCam && h.camConn == conn {
			log.Println("Camera connection closed:", conn.RemoteAddr())
			h.camConn = nil
		} else {
			// Si era un viewer, lo desregistramos
			h.unregister <- conn
		}
		h.mu.Unlock()
		conn.Close() // Asegurar que se cierra al salir del reader
	}()

	// Configurar límites y timeouts si es necesario
	// conn.SetReadLimit(maxMessageSize)
	// conn.SetReadDeadline(time.Now().Add(pongWait))
	// conn.SetPongHandler(...)

	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Error reading from %v: %v", conn.RemoteAddr(), err)
			} else {
				log.Printf("Connection closed for %v: %v", conn.RemoteAddr(), err)
			}
			break // Salir del loop si hay error o cierre
		}

		h.mu.Lock()
		// Identificar si es la cámara (primera conexión o la única binaria)
		// Asumiremos que la cámara es la única que envía mensajes binarios
		if messageType == websocket.BinaryMessage && h.camConn == nil {
			h.camConn = conn
			isCam = true
			log.Println("Camera connection identified:", conn.RemoteAddr())
			// No registrar la cámara como un cliente viewer
			h.mu.Unlock() 
		} else if messageType == websocket.BinaryMessage && h.camConn == conn {
			// Si es la cámara enviando datos binarios (frames)git 
			h.mu.Unlock() // Desbloquear antes de enviar al canal para evitar deadlock
			// Enviar el frame al canal de broadcast (podría bloquear si el buffer está lleno)
			h.broadcast <- message 
		} else if messageType == websocket.TextMessage {
			h.mu.Unlock() // Desbloquear para mensajes de texto
			log.Printf("Received text message from %v: %s", conn.RemoteAddr(), string(message))
			// Podrías manejar comandos de viewers aquí
		} else {
			h.mu.Unlock() // Desbloquear para otros tipos
			log.Printf("Received unexpected message type %d from %v", messageType, conn.RemoteAddr())
		}
	}
}


// Manejador HTTP para las conexiones WebSocket
func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	// Actualizar la conexión HTTP a WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Failed to upgrade connection:", err)
		return
	}
	log.Println("Client connected:", conn.RemoteAddr())

	// Iniciar la goroutine para leer mensajes de esta nueva conexión
	// El reader determinará si es la cámara o un viewer y lo registrará si es viewer
	go hub.reader(conn) 
	
	// Identificación inicial simple: si no hay cámara, asumir que esta *podría* serla
	// El reader lo confirmará al recibir el primer mensaje binario.
	// Si ya hay cámara, registrar este como viewer inmediatamente.
	hub.mu.Lock()
	if hub.camConn != nil {
		hub.mu.Unlock() // Desbloquear antes de enviar al canal
		hub.register <- conn // Registrar como viewer
	} else {
		hub.mu.Unlock()
		log.Println("Potential camera connected, waiting for binary data:", conn.RemoteAddr())
	}
}

func main() {
	hub := newHub()
	go hub.run() // Iniciar el gestor del hub

	// Servir archivos estáticos (HTML/JS del visor) desde la carpeta "public"
	fs := http.FileServer(http.Dir("./public"))
	http.Handle("/", fs)

	// Endpoint para las conexiones WebSocket
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	port := ":8080" // Puerto en el que escuchará el servidor
	log.Printf("WebSocket server starting on port %s", port)
	err := http.ListenAndServe(port, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}