package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"
)

const (
	webhookURL = "http://localhost/webhook"

	secret = "your_real_webhook_secret_here"
)

type Update struct {
	UpdateType string `json:"update_type"`
	Timestamp  int64  `json:"timestamp"`
	ChatID     int64  `json:"chat_id"`
}

func main() {
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 2 * time.Second}

	log.Println("ФАЗА 1: Стресс-тест")
	for i := 1; i <= 100; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sendWebhook(client, Update{
				UpdateType: "new_message",
				Timestamp:  time.Now().UnixMilli(),
				ChatID:     int64(id),
			})
		}(i)
	}

	wg.Wait()
	log.Println("Фаза 1 завершена")

	time.Sleep(2 * time.Second)

	log.Println("ФАЗА 2: Тест Идемпотентности")
	// Имитируем поведение МАХ при нестабильной сети: одно и то же событие прилетает 5 раз
	duplicateUpdate := Update{
		UpdateType: "new_message",
		Timestamp:  1690000000000, // Жестко зафиксированный таймстемп
		ChatID:     999,           // Один и тот же чат
	}

	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sendWebhook(client, duplicateUpdate)
		}()
	}
	wg.Wait()
	log.Println("Фаза 2 завершена")
}

func sendWebhook(client *http.Client, update Update) {
	body, _ := json.Marshal(update)
	req, err := http.NewRequest(http.MethodPost, webhookURL, bytes.NewBuffer(body))
	if err != nil {
		log.Printf("Error forming request: %v", err)
		return
	}

	// Обязательные заголовки
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Max-Bot-Api-Secret", secret)

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Network error: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Warning! expected 200 OK, received: %d", resp.StatusCode)
	}
}
