package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// WebSocketMessage 定义了消息模型
type WebSocketMessage struct {
	Type      string      `json:"type"`                 // "request", "response", "notify", "ping", "pong"
	RequestID string      `json:"request_id,omitempty"` // 请求ID
	Action    string      `json:"action"`               // 操作，比如 "download"
	Data      interface{} `json:"data,omitempty"`       // 消息数据
	Timestamp int64       `json:"timestamp"`            // 时间戳
}

const (
	MessageTypeRequest  = "request"
	MessageTypeResponse = "response"
	MessageTypeNotify   = "notify"
	MessageTypePing     = "ping"
	MessageTypePong     = "pong"
)

// 升级器配置，允许所有跨域请求
var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// handleAgentWs 处理 WebSocket 连接请求
func handleAgentWs(w http.ResponseWriter, r *http.Request) {
	// 读取 token 参数
	token := r.URL.Query().Get("token")
	log.Printf("Go-Agent: new connection established, token: %s", token)

	// 升级 HTTP 连接为 WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}
	defer conn.Close()

	// 设置初始读截止时间，并设置 PongHandler 延长读截止时间
	conn.SetReadDeadline(time.Now().Add(60 * time.Second))
	conn.SetPongHandler(func(appData string) error {
		log.Println("Received pong from client:", appData)
		conn.SetReadDeadline(time.Now().Add(60 * time.Second))
		return nil
	})

	// 后台定时发送 Ping 消息，确保连接保持活跃
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
				log.Println("Ping error:", err)
				return
			}
		}
	}()

	// 循环读取消息
	for {
		msgType, data, err := conn.ReadMessage()
		if err != nil {
			log.Println("Read error:", err)
			break
		}
		if msgType != websocket.TextMessage {
			continue
		}
		var msg WebSocketMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			log.Println("Unmarshal error:", err)
			continue
		}
		// 如果收到 Ping 消息（一般由对端主动发送），这里可以忽略，底层 Ping/Pong 机制会自动处理

		// 处理下载请求消息
		if msg.Type == MessageTypeRequest && msg.Action == "download" {
			log.Printf("Received download request, RequestID: %s", msg.RequestID)
			// 模拟下载处理（实际业务逻辑可自行替换）
			time.Sleep(2 * time.Second)
			response := WebSocketMessage{
				Type:      MessageTypeResponse,
				RequestID: msg.RequestID,
				Action:    "download",
				Data:      "Download successful",
				Timestamp: time.Now().Unix(),
			}
			respData, err := json.Marshal(response)
			if err != nil {
				log.Println("Marshal response error:", err)
				continue
			}
			conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, respData); err != nil {
				log.Println("Write response error:", err)
				break
			}
		} else {
			// 其他消息仅打印日志（或根据业务扩展处理）
			log.Printf("Received message: %+v", msg)
		}
	}
}

func main() {
	http.HandleFunc("/ws", handleAgentWs)
	log.Println("Go-Agent running on :8888")
	if err := http.ListenAndServe(":8888", nil); err != nil {
		log.Fatal("ListenAndServe error:", err)
	}
}
