package main

import (
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"go-websocket-server/api"   // Import the api package
	"go-websocket-server/utils" // Import utils for DB initialization
	"log"
	"net/http"
)

// Upgrader for handling WebSocket connections.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins
	},
}

// Message structure to define the shape of messages passed between the server and the frontend.
type Message struct {
	Text           string `json:"text"`
	ConversationID string `json:"conversationId"`
	Type           string `json:"type"`
}

func main() {
	// Initialize the SQLite database.
	utils.InitDB("./conversation.db")
	// Handle WebSocket connections at the /ws endpoint.
	http.HandleFunc("/ws", handleWebSocket)

	fmt.Println("Server is running on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// These three goroutines handle sending data back to the user:
// SendTranscriptToClient - Streams STT data from the deepgram websocket as a user message
// SendTextToClient - Streams text from Groq as a bot message
// SendAudioToClient - Sends audio from deepgram as a single file
func makeTurnChannels(userTranscript chan string,
	writeChan chan utils.WebSocketPacket,
	stopChan chan bool) (userMessage chan string,
	botTextForClient chan string,
	botTextForTTS chan string,
) {
	userMessage = make(chan string) // Channel for entire user transcript as a single string
	go api.SendTranscriptToClient(userTranscript, userMessage, writeChan, stopChan)

	botAudio := make(chan []byte)
	go api.SendAudioToClient(botAudio, writeChan)

	botTextForClient = make(chan string)
	botTextForTTS = make(chan string)
	go api.BufferTextForTTS(botTextForTTS, botAudio)
	go api.SendTextToClient(botTextForClient, writeChan)
	return userMessage, botTextForClient, botTextForTTS
}

// handleWebSocket handles incoming WebSocket data packets.
func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil) // Upgrade to a WebSocket connection.
	if err != nil {
		log.Println(err)
		return
	}
	// Single channel for outbound data on the websocket
	writeChan := make(chan utils.WebSocketPacket)
	go utils.WriteToWebsocket(writeChan, conn)
	// Initialize Deepgram WebSocket connection
	// and channel to hold the user transcript stream
	userTranscript := make(chan string) // channel for streaming audio transcript
	stopChan := make(chan bool)
	// Part of initializing the deepgram connection is listening for packets
	// and sending them to the userTranscript channel
	deepgramConn, err := api.NewDeepgramConnection(userTranscript, stopChan)
	if err != nil {
		log.Fatalf("Failed to connect to Deepgram: %v", err)
	}
	defer conn.Close() // Ensure the connection is closed when done.
	userMessage, botTextForClient, botTextForTTS := makeTurnChannels(userTranscript, writeChan, stopChan)

	for {
		messageType, p, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}

		if messageType == websocket.TextMessage {
			var message Message
			if err := json.Unmarshal(p, &message); err != nil {
				log.Println("Error unmarshaling message:", err)
				continue
			}
			if message.Type == "audioEnd" {
				log.Println("Received audioEnd message, waiting for final transcripts")
				// Send a special Finalize message to Deepgram
				log.Println("Finalizing deepgram transcription")
				m := "{\"type\":\"Finalize\"}"
				deepgramConn.WriteMessage(websocket.TextMessage, []byte(m))
				log.Println("Sending stop signal..")
				stopChan <- true // tell the listener to stop
				close(stopChan)
				// Wait for all transcripts to be processed and returned
				// This is taking waaaay too long!!
				log.Println("Compiling full transcript...")
				fullTranscript := <-userMessage
				message.Text = fullTranscript
				log.Printf("Full transcript is %s", fullTranscript)
			}
			log.Printf("Sending text to llama: %s", message.Text)

			if message.ConversationID == "" {
				log.Println("Error: ConversationID is empty")
				continue
			}
			go api.AskLlama(message.ConversationID, message.Text, botTextForClient, botTextForTTS)
			// Re-open these two channels
			log.Println("Re-opening channels")
			userTranscript = make(chan string)
			stopChan = make(chan bool)
			userMessage, botTextForClient, botTextForTTS = makeTurnChannels(userTranscript, writeChan, stopChan)
			deepgramConn, err = api.NewDeepgramConnection(userTranscript, stopChan)

		} else if messageType == websocket.BinaryMessage {
			log.Printf("Received %d bytes of audio data", len(p))

			// Send the audio chunk to Deepgram directly
			err := deepgramConn.WriteMessage(websocket.BinaryMessage, p)
			if err != nil {
				// Reconnect and try again
				deepgramConn, err = api.NewDeepgramConnection(userTranscript, stopChan)
				if err != nil {
					log.Fatalf("Failed to connect to Deepgram: %v", err)
				} else {
					err := deepgramConn.WriteMessage(websocket.BinaryMessage, p)
					if err != nil {
						log.Fatal("Failed to re-connect to Deepgram:", err)
					} else {
						log.Println("Successfully sent chunk to deepgram on the second try")
					}
				}
			} else {
				log.Println("Successfully sent chunk to Deepgram")
			}
		}
	}
}
