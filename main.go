package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	nativeLanguage        = "ES"    // idioma al que se traduce lo que dicen los demás
	defaultOutgoingLang   = "EN-US" // si el script no manda ?lang=
	maxIncomingBodyBytes  = 4096    // el chat de SL es mucho más corto que esto
	maxCacheEntries       = 500     // al llenarse, la caché se vacía entera (simple y barato)
	maxCachedTextBytes    = 80      // solo se cachean frases cortas, que son las que se repiten
	minLettersToTranslate = 2
)

var (
	deepLClient = &http.Client{Timeout: 20 * time.Second}

	// Enlaces: no se traducen y DeepL los cobraría carácter a carácter.
	linkPattern = regexp.MustCompile(`(?i)(?:https?://|secondlife://|www\.)\S+`)

	// Palabras de relleno (risas, abreviaturas) que no merece la pena mandar a DeepL.
	// Un mensaje se descarta solo si TODAS sus palabras son de este tipo.
	fillerWordPattern = regexp.MustCompile(`^(?:(?:ha|he|hi|ja|je|ji|ah|eh)+h?|x+d+|l+o+l+|lmao+|rofl|o+k+|okay|k+|hm+|omg|brb|afk|ty|np)$`)

	cache = translationCache{entries: make(map[string]cachedTranslation)}
)

// ---------- Tipos de DeepL ----------

type deepLTranslateRequest struct {
	Text       []string `json:"text"`
	TargetLang string   `json:"target_lang"`
}

type deepLTranslateResponse struct {
	Translations []struct {
		DetectedSourceLanguage string `json:"detected_source_language"`
		Text                   string `json:"text"`
	} `json:"translations"`
}

type deepLUsageResponse struct {
	CharacterCount int64 `json:"character_count"`
	CharacterLimit int64 `json:"character_limit"`
}

// ---------- Caché en memoria ----------
// Se pierde cuando Render reinicia o duerme el servicio; no pasa nada.

type cachedTranslation struct {
	text         string
	detectedLang string
}

type translationCache struct {
	mu      sync.Mutex
	entries map[string]cachedTranslation
}

func (c *translationCache) get(key string) (cachedTranslation, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, found := c.entries[key]
	return entry, found
}

func (c *translationCache) put(key string, entry cachedTranslation) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCacheEntries {
		c.entries = make(map[string]cachedTranslation)
	}
	c.entries[key] = entry
}

// ---------- Llamadas a DeepL ----------

// Las claves del plan Free terminan en ":fx" y van a api-free.deepl.com;
// las de pago van a api.deepl.com. Si se mezclan, DeepL rechaza la petición.
func deepLBaseURL(apiKey string) string {
	if strings.HasSuffix(apiKey, ":fx") {
		return "https://api-free.deepl.com/v2"
	}
	return "https://api.deepl.com/v2"
}

func callDeepL(method, path string, payload []byte) ([]byte, error) {
	apiKey := strings.TrimSpace(os.Getenv("DEEPL_API_KEY"))
	if apiKey == "" {
		return nil, fmt.Errorf("falta la variable de entorno DEEPL_API_KEY")
	}

	var requestBody io.Reader
	if payload != nil {
		requestBody = bytes.NewReader(payload)
	}

	req, err := http.NewRequest(method, deepLBaseURL(apiKey)+path, requestBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "DeepL-Auth-Key "+apiKey)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := deepLClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("no se pudo contactar con DeepL: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error leyendo respuesta de DeepL: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("DeepL respondió %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func translate(text, targetLang string) (cachedTranslation, error) {
	payload, err := json.Marshal(deepLTranslateRequest{Text: []string{text}, TargetLang: targetLang})
	if err != nil {
		return cachedTranslation{}, err
	}

	body, err := callDeepL(http.MethodPost, "/translate", payload)
	if err != nil {
		return cachedTranslation{}, err
	}

	var parsed deepLTranslateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return cachedTranslation{}, fmt.Errorf("respuesta de DeepL ilegible: %w", err)
	}
	if len(parsed.Translations) == 0 {
		return cachedTranslation{}, fmt.Errorf("DeepL no devolvió traducciones")
	}

	first := parsed.Translations[0]
	return cachedTranslation{
		text:         first.Text,
		detectedLang: strings.ToUpper(first.DetectedSourceLanguage),
	}, nil
}

func translateWithCache(text, targetLang string) (cachedTranslation, error) {
	cacheable := len(text) <= maxCachedTextBytes
	cacheKey := targetLang + "|" + strings.ToLower(text)

	if cacheable {
		if entry, found := cache.get(cacheKey); found {
			return entry, nil
		}
	}

	entry, err := translate(text, targetLang)
	if err == nil && cacheable {
		cache.put(cacheKey, entry)
	}
	return entry, err
}

// ---------- Filtros previos (no gastan cuota) ----------

func removeLinks(text string) string {
	return strings.TrimSpace(linkPattern.ReplaceAllString(text, " "))
}

// false si el mensaje no tiene letras suficientes o solo son risas/abreviaturas.
func needsTranslation(text string) bool {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) })

	letterCount := 0
	onlyFiller := true
	for _, word := range words {
		letterCount += len([]rune(word))
		if onlyFiller && !fillerWordPattern.MatchString(word) {
			onlyFiller = false
		}
	}
	return letterCount >= minLettersToTranslate && !onlyFiller
}

// ---------- HTTP ----------

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
// Respuestas para el script:
//
//	200 con texto -> traducción
//	200 vacío     -> descartado sin llamar a DeepL (risas, enlaces, sin letras)
//	204           -> estaba en español (el script recuerda a ese hablante)
func translateInHandler(w http.ResponseWriter, r *http.Request) {
	text, ok := readChatText(w, r)
	if !ok {
		return
	}

	text = removeLinks(text)
	if !needsTranslation(text) {
		w.WriteHeader(http.StatusOK)
		return
	}

	result, err := translateWithCache(text, nativeLanguage)
	if err != nil {
		log.Printf("[in] %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	if result.detectedLang == nativeLanguage {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	writePlainText(w, fmt.Sprintf("[%s] %s", result.detectedLang, result.text))
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

	result, err := translateWithCache(text, targetLang)
	if err != nil {
		log.Printf("[out] %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	writePlainText(w, result.text)
}

// Consumo de DeepL en el periodo actual (para /7 !uso).
func usageHandler(w http.ResponseWriter, r *http.Request) {
	body, err := callDeepL(http.MethodGet, "/usage", nil)
	if err != nil {
		log.Printf("[usage] %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	var usage deepLUsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		http.Error(w, "respuesta de uso ilegible", http.StatusBadGateway)
		return
	}

	percent := 0.0
	if usage.CharacterLimit > 0 {
		percent = float64(usage.CharacterCount) * 100 / float64(usage.CharacterLimit)
	}
	writePlainText(w, fmt.Sprintf("DeepL: %d / %d caracteres usados en este periodo (%.1f%%)",
		usage.CharacterCount, usage.CharacterLimit, percent))
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
	http.HandleFunc("/usage", usageHandler)
	http.HandleFunc("/translate-in", translateInHandler)
	http.HandleFunc("/translate-out", translateOutHandler)

	log.Printf("Servidor traductor escuchando en el puerto %s...", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
