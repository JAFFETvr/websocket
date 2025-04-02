package main

import (
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// Ajustar buffers para Base64 (son más grandes que binario)
var upgrader = websocket.Upgrader{
	ReadBufferSize:  10240, // Aumentado
	WriteBufferSize: 10240, // Aumentado
	CheckOrigin:     func(r *http.Request) bool { return true }, // Permite conexiones de cualquier origen
}

// Estructura para gestionar clientes conectados
type Hub struct {
	clients    map[*websocket.Conn]bool // Clientes registrados (viewers)
	broadcast  chan []byte              // Canal para enviar mensajes (frames Base64 como []byte de texto) a los viewers
	register   chan *websocket.Conn     // Canal para registrar viewers
	unregister chan *websocket.Conn     // Canal para desregistrar viewers
	camConn    *websocket.Conn          // Referencia a la conexión de la cámara (asumimos una sola cámara)
	mu         sync.Mutex               // Mutex para proteger el acceso a camConn y clients
}

func newHub() *Hub {
	return &Hub{
		clients:    make(map[*websocket.Conn]bool),
		broadcast:  make(chan []byte, 10), // Buffer un poco más grande
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
			if _, ok := h.clients[conn]; !ok { // Evitar registrar dos veces
				h.clients[conn] = true
				log.Println("Viewer registered:", conn.RemoteAddr())
			}
			h.mu.Unlock()

		// Un viewer o la cámara se desconecta
		case conn := <-h.unregister:
			h.mu.Lock()
			// Si es un viewer registrado, eliminarlo
			if _, ok := h.clients[conn]; ok {
				delete(h.clients, conn)
				conn.Close() // Asegurarse de cerrar la conexión
				log.Println("Viewer unregistered:", conn.RemoteAddr())
			}
			// Si era la cámara la que se desconectó, limpiar la referencia
			if h.camConn == conn {
				log.Println("Camera unregistered/disconnected:", conn.RemoteAddr())
				h.camConn = nil // Permite que una nueva conexión sea identificada como cámara
				// No necesitamos cerrar conn aquí de nuevo si ya se hizo arriba o en el reader
			}
			h.mu.Unlock()

		// Mensaje recibido (frame Base64 como []byte) y listo para enviar a viewers
		case message := <-h.broadcast: // message es []byte conteniendo TEXTO Base64
			h.mu.Lock()
			// Enviar a todos los viewers registrados
			activeViewers := len(h.clients)
			// log.Printf("Broadcasting message (size: %d) to %d viewers", len(message), activeViewers) // Log detallado opcional
			if activeViewers > 0 {
				for conn := range h.clients {
					// Usar goroutine para no bloquear el broadcast si un cliente es lento
					// Pasar una copia del mensaje a la goroutine
					msgCopy := make([]byte, len(message))
					copy(msgCopy, message)

					go func(c *websocket.Conn, msgToSend []byte) {
						h.mu.Lock() // Bloquear brevemente para verificar existencia
						_, clientExists := h.clients[c]
						h.mu.Unlock()

						if clientExists { // Solo enviar si todavía está registrado
							// *** ENVIAR COMO MENSAJE DE TEXTO ***
							// Establecer un timeout de escritura puede ser útil
							// c.SetWriteDeadline(time.Now().Add(10 * time.Second)) // Ejemplo
							err := c.WriteMessage(websocket.TextMessage, msgToSend)
							if err != nil {
								log.Printf("Error sending TEXT to viewer %v: %v. Unregistering.", c.RemoteAddr(), err)
								// Si hay error al escribir, asumir que el cliente se desconectó
								h.unregister <- c // Pedir que se desregistre
							}
						}
					}(conn, msgCopy) // Pasar la copia
				}
			}
			h.mu.Unlock()
		}
	}
}

// Lee mensajes de una conexión específica (cámara o viewer)
func (h *Hub) reader(conn *websocket.Conn) {
	isCam := false // Flag para saber si esta conexión fue identificada como la cámara

	// Asegurar limpieza al salir de la función reader (por cierre o error)
	defer func() {
		h.mu.Lock()
		// Si era la cámara, limpiamos la referencia global para que otra pueda conectar
		if isCam && h.camConn == conn {
			log.Println("Camera connection closed (reader exit):", conn.RemoteAddr())
			h.camConn = nil
		}
		// Intentar desregistrar esta conexión (sea cámara o viewer)
		// La lógica en run() se encargará de eliminarla del mapa de viewers si existe
		h.unregister <- conn
		h.mu.Unlock()
		conn.Close() // Asegurar que la conexión websocket se cierra
		log.Printf("Reader goroutine for %v finished.", conn.RemoteAddr())
	}()

	// Configurar límites y timeouts si es necesario
	// conn.SetReadLimit(maxMessageSize) // Podría necesitar ser más grande para Base64
	// conn.SetReadDeadline(time.Now().Add(60 * time.Second)) // Ejemplo: cerrar si no hay mensajes en 60s
	// conn.SetPongHandler(func(string) error { conn.SetReadDeadline(time.Now().Add(60 * time.Second)); return nil }) // Resetear deadline al recibir Pong

	// Loop de lectura de mensajes
	for {
		messageType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure) {
				log.Printf("Reader Error reading from %v: %v", conn.RemoteAddr(), err)
			} else if err == websocket.ErrCloseSent {
				log.Printf("Reader closing for %v because close frame was sent.", conn.RemoteAddr())
			} else {
				// Incluye timeouts si se configuran SetReadDeadline
				log.Printf("Reader Connection closed/error for %v: %v", conn.RemoteAddr(), err)
			}
			break // Salir del loop si hay error o cierre
		}

		// --- LÓGICA DE PROCESAMIENTO ---
		h.mu.Lock() // Bloquear para acceso seguro a variables compartidas (camConn)

		// 1. Identificar si es la cámara (si aún no tenemos una)
		//    Asume que la cámara será la primera en enviar un mensaje de TEXTO.
		//    ¡¡¡ADVERTENCIA: Esto es frágil!!! Un viewer podría enviar texto primero.
		//    Una mejor solución es un handshake (ej: ESP32 envía JSON `{"type":"cam_hello"}`)
		if h.camConn == nil && messageType == websocket.TextMessage {
			// Podrías añadir más validaciones aquí (longitud mínima, prefijo específico?)
			// if len(message) > 1000 { // Ejemplo de validación simple por tamaño
			h.camConn = conn
			isCam = true // Marcar esta conexión como la cámara para el 'defer'
			log.Println("Camera connection identified (via TextMessage):", conn.RemoteAddr())
			// No registrar la cámara como un viewer normal
			// } else {
			//  log.Printf("Received short TextMessage from potential camera %v, ignoring as handshake.", conn.RemoteAddr())
			// }
			// Proceder a tratar este mensaje como un frame (caerá en el siguiente bloque 'if')
		}

		// 2. Procesar mensaje según el tipo y si es la cámara
		if messageType == websocket.TextMessage {
			if h.camConn == conn { // Es la cámara identificada enviando un frame Base64
				// Desbloquear ANTES de enviar al canal broadcast para evitar deadlocks
				// y permitir que 'run' procese mientras leemos el siguiente mensaje.
				h.mu.Unlock()

				// Enviar el frame (como bytes de texto Base64) al canal de broadcast
				// Es más seguro enviar una copia
				msgCopy := make([]byte, len(message))
				copy(msgCopy, message)
				// log.Printf("Received Base64 frame from camera %v (size: %d). Sending to broadcast.", conn.RemoteAddr(), len(msgCopy)) // Log detallado opcional
				
				// Enviar al canal. Podría bloquear si el buffer del canal está lleno.
				select {
				 case h.broadcast <- msgCopy:
					 // Mensaje enviado al canal
				 default:
					 log.Printf("WARN: Broadcast channel full. Dropping frame from camera %v.", conn.RemoteAddr())
					 // Considerar métricas o lógica de backpressure aquí si esto ocurre a menudo
				}

			} else { // Es un mensaje de texto, pero NO de la cámara (probablemente un viewer)
				viewerMsg := string(message) // Copiar antes de desbloquear
				h.mu.Unlock()
				log.Printf("Received text message from VIEWER %v: %s", conn.RemoteAddr(), viewerMsg)
				// Aquí podrías manejar comandos enviados por los viewers (ej: controlar LED, etc.)
			}
		} else if messageType == websocket.BinaryMessage { // Inesperado ahora que usamos Base64
			msgLen := len(message)
			h.mu.Unlock() // Desbloquear antes de loggear
			log.Printf("Received UNEXPECTED Binary message from %v, Size: %d. Ignoring.", conn.RemoteAddr(), msgLen)
		} else {
			// Otros tipos de mensajes (Ping/Pong son manejados por la librería usualmente, Close ya se maneja en el error)
			h.mu.Unlock() // Desbloquear
			log.Printf("Received unhandled message type %d from %v", messageType, conn.RemoteAddr())
		}
		// Fin del bloque de procesamiento, el loop continúa para leer el siguiente mensaje
	} // Fin del for loop
}

// Manejador HTTP para las conexiones WebSocket
func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	// Actualizar la conexión HTTP a WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Failed to upgrade connection:", err)
		return
	}
	// ¡No cerrar 'conn' aquí! El 'reader' se encarga de eso en su 'defer'.
	log.Println("Client connected:", conn.RemoteAddr())

	// Iniciar la goroutine para leer mensajes de esta nueva conexión.
	// El 'reader' ahora se encarga de identificar si es cámara o viewer
	// y de llamar a 'unregister' cuando la conexión termina.
	go hub.reader(conn)

	// Ya no hacemos la identificación/registro aquí. El reader lo maneja.
	log.Printf("Connection %v handed off to reader goroutine for role identification.", conn.RemoteAddr())

	// Registrar inmediatamente como viewer (siempre) - El reader identificará la cámara y no la borrará
	// Opcional: Registrar todos como viewers inicialmente, la cámara nunca enviará
	// mensajes que la hub.run() procesaría si no está en el mapa h.clients
	hub.register <- conn

}

func main() {
	hub := newHub()
	go hub.run() // Iniciar el gestor del hub en su propia goroutine

	// Servir archivos estáticos (HTML/JS del visor) desde la carpeta "public"
	// Asegúrate de tener una carpeta 'public' con tu index.html, etc.
	fs := http.FileServer(http.Dir("./public"))
	http.Handle("/", fs) // Servir archivos estáticos en la raíz

	// Endpoint para las conexiones WebSocket
	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		serveWs(hub, w, r)
	})

	port := ":8080" // Puerto en el que escuchará el servidor
	log.Printf("WebSocket server (expecting Base64 Text) starting on port %s", port)
	err := http.ListenAndServe(port, nil) // Iniciar servidor HTTP
	if err != nil {
		log.Fatal("ListenAndServe Error: ", err)
	}
}