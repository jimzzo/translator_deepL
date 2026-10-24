package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
)

type LSLRequest struct {
	Text       string `json:"text"`
	TargetLang string `json:"target_lang"`
}

type DeepLRequest struct {
	Text       []string `json:"text"`
	TargetLang string   `json:"target_lang"`
}

type DeepLResponse struct {
	Translations []struct {
		DetectedSourceLanguage string `json:"detected_source_language"`
		Text                   string `json:"text"`
	} `json:"translations"`
}

func callDeepL(text string, targetLang string, apiKey string) (*DeepLResponse, error) {
	deepLPayload := DeepLRequest{
		Text:       []string{text},
		TargetLang: targetLang,
	}

	payloadBytes, _ := json.Marshal(deepLPayload)
	req, err := http.NewRequest("POST", "https://api-free.deepl.com/v2/translate", bytes.NewBuffer(payloadBytes))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", "DeepL-Auth-Key "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("error HTTP en DeepL")
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	var deeplResp DeepLResponse
	json.Unmarshal(body, &deeplResp)

	return &deeplResp, nil
}

// Maneja los mensajes ENTRANTES de otros avatares
func translateInHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}

	var req LSLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "Petición inválida", http.StatusBadRequest)
		return
	}

	apiKey := os.Getenv("DEEPL_API_KEY")
	if apiKey == "" {
		http.Error(w, "Falta API Key", http.StatusInternalServerError)
		return
	}

	// Idioma nativo fijo: Español (ES)
	resp, err := callDeepL(req.Text, "ES", apiKey)
	if err != nil || len(resp.Translations) == 0 {
		http.Error(w, "Error al traducir", http.StatusInternalServerError)
		return
	}

	detectedLang := resp.Translations[0].DetectedSourceLanguage

	// Si el mensaje de origen YA ESTÁ en español, no devolvemos texto (204 No Content)
	if detectedLang == "ES" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Devolver traducción indicando el idioma de origen
	resultText := fmt.Sprintf("[%s]: %s", detectedLang, resp.Translations[0].Text)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(resultText))
}

// Maneja tus mensajes SALIENTES enviando al chat público
func translateOutHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}

	var req LSLRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		http.Error(w, "Petición inválida", http.StatusBadRequest)
		return
	}

	if req.TargetLang == "" {
		req.TargetLang = "EN" // Idioma saliente por defecto: Inglés
	}

	apiKey := os.Getenv("DEEPL_API_KEY")
	if apiKey == "" {
		http.Error(w, "Falta API Key", http.StatusInternalServerError)
		return
	}

	resp, err := callDeepL(req.Text, req.TargetLang, apiKey)
	if err != nil || len(resp.Translations) == 0 {
		http.Error(w, "Error al traducir", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(resp.Translations[0].Text))
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.HandleFunc("/translate-in", translateInHandler)
	http.HandleFunc("/translate-out", translateOutHandler)

	fmt.Printf("Servidor traductor escuchando en el puerto %s...\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
