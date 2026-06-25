package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
)

const port = ":8080"

// структура для передачи метрик на ПК
type ProgressState struct {
	sync.Mutex
	TotalFiles     int    `json:"total_files"`
	CurrentFileNum int    `json:"current_file_num"`
	IsActive       bool   `json:"is_active"`
	CurrentSpeed   string `json:"current_speed"`
	TimeLeft       string `json:"time_left"`
}

var state = &ProgressState{}

// автоматическое добавление правила в Брандмауэр Windows
func allowFirewall() {
	cmd := exec.Command("netsh", "advfirewall", "firewall", "add", "rule",
		"name=GoPhotoTransfer", "dir=in", "action=allow", "protocol=TCP", "localport=8080")
	_ = cmd.Run()
}

// определение локального IP-адреса ПК в сети Wi-Fi (обход VPN/нет)
func getLocalIP() string {
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		return "127.0.0.1"
	}
	defer conn.Close()
	localAddr := conn.LocalAddr().(*net.UDPAddr)
	return localAddr.IP.String()
}

func main() {
	// получение абсолютного путь к папке uploads для проводника
	targetDir, _ := filepath.Abs("./uploads")
	os.MkdirAll(targetDir, os.ModePerm)

	// Настраиваем сеть
	allowFirewall()

	// открытие браузер на ПК через полсекунды после старта
	go func() {
		time.Sleep(500 * time.Millisecond)
		exec.Command("cmd", "/c", "start", "http://localhost:8080").Run()
	}()

	http.Handle("/", http.FileServer(http.Dir("./static")))

	// получения IP и URL для телефона
	http.HandleFunc("/api/info", func(w http.ResponseWriter, r *http.Request) {
		localIP := getLocalIP()
		serverURL := fmt.Sprintf("http://%s%s/mobile.html", localIP, port)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": serverURL, "ip": localIP})
	})

	// генерации QR-кода
	http.HandleFunc("/api/qr", func(w http.ResponseWriter, r *http.Request) {
		url := r.URL.Query().Get("url")
		if url == "" {
			http.Error(w, "Missing url parameter", http.StatusBadRequest)
			return
		}
		png, err := qrcode.Encode(url, qrcode.Medium, 256)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Write(png)
	})

	// открытие папки в Проводнике Windows
	http.HandleFunc("/api/open-folder", func(w http.ResponseWriter, r *http.Request) {
		cmd := exec.Command("explorer", targetDir)
		err := cmd.Run()
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "success"})
	})

	// точка для телефона сообщает общее количество файлов
	http.HandleFunc("/api/start", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Count int `json:"count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		state.Lock()
		state.TotalFiles = req.Count
		state.CurrentFileNum = 0
		state.IsActive = true
		state.CurrentSpeed = ""
		state.TimeLeft = ""
		state.Unlock()
		w.WriteHeader(http.StatusOK)
	})

	// потоковый стриминг на диск
	http.HandleFunc("/api/upload", uploadHandler)

	// телефон шлет телеметрию скорости
	http.HandleFunc("/api/telemetry", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Speed string `json:"speed"`
			Eta   string `json:"eta"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil {
			state.Lock()
			state.CurrentSpeed = req.Speed
			state.TimeLeft = req.Eta
			state.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	})

	// опрос статуса для прогресс-бара
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		state.Lock()
		defer state.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(state)
	})

	// выключения сервера и закрытия консольного окна exe
	http.HandleFunc("/api/shutdown", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"shutdown"}`))
		go func() {
			time.Sleep(500 * time.Millisecond)
			os.Exit(0)
		}()
	})

	fmt.Printf("Сервер успешно запущен на http://localhost%s\n", port)
	log.Fatal(http.ListenAndServe("0.0.0.0"+port, nil))
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// чтение файлов потоком
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if part.FormName() != "photo" {
			part.Close()
			continue
		}

		filename := part.FileName()
		if filename == "" {
			part.Close()
			continue
		}

		dst, err := os.Create(filepath.Join("./uploads", filename))
		if err != nil {
			part.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		if _, err := io.Copy(dst, part); err != nil {
			dst.Close()
			part.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		dst.Close()
		part.Close()
	}

	state.Lock()
	state.CurrentFileNum++
	if state.CurrentFileNum >= state.TotalFiles {
		state.IsActive = false
	}
	state.Unlock()

	w.WriteHeader(http.StatusOK)
}
