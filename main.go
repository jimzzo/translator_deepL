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
	"time"
)

const (
	nativeLanguage       = "ES"    // idioma al que se traduce lo que dicen los demás
	defaultOutgoingLang  = "EN-US" // si el script no manda ?lang=
	maxIncomingBodyBytes = 4096    // el chat de SL es mucho más corto que esto
)

var deepLClient = &http.Client{Timeout: 20 * time.Second}

type deepLRequest struct {
	Text       []string `json:"text"`
	TargetLang string   `json:"target_lang"`
}

type deepLResponse struct {
	Translations []struct {
		DetectedSourceLanguage string `json:"detected_source_language"`
		Text                   string `json:"text"`
	} `json:"translations"`
}

// Las claves del plan Free terminan en ":fx" y van a api-free.deepl.com;
// las de pago van a api.deepl.com. Si se mezclan, DeepL rechaza la petición.
func deepLEndpoint(apiKey string) string {
	if strings.HasSuffix(apiKey, ":fx") {
		return "https://api-free.deepl.com/v2/translate"
	}
	return "https://api.deepl.com/v2/translate"
}

func translate(text, targetLang string) (translated, detectedLang string, err error) {
	apiKey := strings.TrimSpace(os.Getenv("DEEPL_API_KEY"))
	if apiKey == "" {
		return "", "", fmt.Errorf("falta la variable de entorno DEEPL_API_KEY")
	}

	payload, err := json.Marshal(deepLRequest{Text: []string{text}, TargetLang: targetLang})
	if err != nil {
		return "", "", err
	}

	req, err := http.NewRequest(http.MethodPost, deepLEndpoint(apiKey), bytes.NewReader(payload))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := deepLClient.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("no se pudo contactar con DeepL: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", "", fmt.Errorf("error leyendo respuesta de DeepL: %w", err)
	}

	// Antes este error se tragaba sin decir nada; ahora se ve el código y el motivo.
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("DeepL respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed deepLResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("respuesta de DeepL ilegible: %w", err)
	}
	if len(parsed.Translations) == 0 {
		return "", "", fmt.Errorf("DeepL no devolvió traducciones")
	}

	first := parsed.Translations[0]
	return first.Text, strings.ToUpper(first.DetectedSourceLanguage), nil
}

// El script LSL manda el mensaje tal cual, en texto plano (sin JSON).
func readChatText(w http.ResponseWriter, r *http.Request) (string, bool) {
	if r.Method != http.MethodPost {
		http.Error(w, "usa POST", http.StatusMethodNotAllowed)
		return "", false
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxIncomingBodyBytes))
	if err != nil {
		http.Error(w, "cuerpo demasiado grande o ilegible", http.StatusBadRequest)
		return "", false
	}

	text := strings.TrimSpace(string(raw))
	if text == "" {
		http.Error(w, "texto vacío", http.StatusBadRequest)
		return "", false
	}
	return text, true
}

func writePlainText(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	io.WriteString(w, text)
}

// Mensajes ENTRANTES de otros avatares -> español.
func translateInHandler(w http.ResponseWriter, r *http.Request) {
	text, ok := readChatText(w, r)
	if !ok {
		return
	}

	translated, detected, err := translate(text, nativeLanguage)
	if err != nil {
		log.Printf("[in] %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Ya estaba en español: no hay nada que mostrar.
	if detected == nativeLanguage {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writePlainText(w, fmt.Sprintf("[%s] %s", detected, translated))
}

// Tus mensajes SALIENTES -> idioma indicado en ?lang=
func translateOutHandler(w http.ResponseWriter, r *http.Request) {
	text, ok := readChatText(w, r)
	if !ok {
		return
	}

	targetLang := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("lang")))
	if targetLang == "" {
		targetLang = defaultOutgoingLang
	}

	translated, _, err := translate(text, targetLang)
	if err != nil {
		log.Printf("[out] %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writePlainText(w, translated)
}

// Ping ligero para despertar / mantener despierto el servicio en Render.
func healthHandler(w http.ResponseWriter, r *http.Request) {
	writePlainText(w, "ok")
}

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if os.Getenv("DEEPL_API_KEY") == "" {
		log.Println("AVISO: DEEPL_API_KEY no está definida, todas las traducciones fallarán")
	}

	http.HandleFunc("/health", healthHandler)
	http.HandleFunc("/translate-in", translateInHandler)
	http.HandleFunc("/translate-out", translateOutHandler)

	log.Printf("Servidor traductor escuchando en el puerto %s...", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
