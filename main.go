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
	maxBatchBodyBytes     = 32768   // lote de mensajes entrantes (el script manda bastante menos)
	maxBatchMessages      = 50
	maxCacheEntries       = 500 // al llenarse, la caché se vacía entera (simple y barato)
	maxCachedTextBytes    = 80  // solo se cachean frases cortas, que son las que se repiten
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

	// Palabras claramente españolas: con una basta como indicio.
	strongSpanishWords = wordSet(`hola holi buenas buenos dias días tardes noches gracias adios adiós chao chau
		estas estás estoy estamos bien muy mucho mucha muchos muchas quiero quieres tengo tienes puedo puedes
		eres soy somos también tambien aquí aqui allí alli ahora hoy mañana ayer porque porqué dónde donde
		cuándo cuando qué cómo quién quien vale claro bueno nada todo todos ustedes usted vosotros ella ellos
		gusta gusto encanta amigo amiga amigos amigas guapa guapo linda lindo hermosa hermoso beso besos abrazo
		abrazos jaja jajaja jeje oye mira vamos venga perdón perdon siempre nunca algo alguien gente cosa
		bailar baila bailamos música musica canción cancion bienvenido bienvenida bienvenidos saludos feliz
		cariño verdad entonces pues así asi eso esto ese esa este también sí pero muy`)

	// Palabras cortas que también existen en portugués, italiano o francés:
	// suman, pero por sí solas no deciden.
	weakSpanishWords = wordSet(`como esta está que para por si el la lo los las del al un una unos unas y o
		de en con sin me te se nos le les ya mi mis tu tú tus su sus es son yo él mas más menos no a`)
)

// Reduce a una las letras repetidas 3 o más veces seguidas ("holaaa" -> "hola").
// "perro" o "llama" no cambian. (El regexp de Go no admite \1, por eso va a mano.)
func collapseRepeatedLetters(word string) string {
	runes := []rune(word)
	var result []rune
	for i := 0; i < len(runes); {
		j := i
		for j < len(runes) && runes[j] == runes[i] {
			j++
		}
		if j-i >= 3 {
			result = append(result, runes[i])
		} else {
			result = append(result, runes[i:j]...)
		}
		i = j
	}
	return string(result)
}

func wordSet(words string) map[string]bool {
	set := make(map[string]bool)
	for _, word := range strings.Fields(words) {
		set[word] = true
	}
	return set
}

func splitWords(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) })
}

// Comprobación local, sin gastar cuota de DeepL. DeepL falla a menudo con
// mensajes cortos (p. ej. "Hola" lo detectó como [ST]), así que primero se mira aquí.
func looksSpanish(text string) bool {
	if strings.ContainsAny(text, "¿¡ñÑ") {
		return true
	}

	words := splitWords(text)
	if len(words) == 0 {
		return false
	}

	strongCount, spanishCount := 0, 0
	for _, word := range words {
		word = collapseRepeatedLetters(word)
		if strongSpanishWords[word] {
			strongCount++
			spanishCount++
		} else if weakSpanishWords[word] {
			spanishCount++
		}
	}
	return strongCount > 0 && spanishCount*100 >= len(words)*60
}

// Solo letras, en minúsculas: para comparar original y traducción.
func lettersOnly(text string) string {
	return strings.Join(splitWords(text), "")
}

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

// Varios textos en UNA sola llamada a DeepL. DeepL devuelve las
// traducciones en el mismo orden en que se le mandan.
func translateMany(texts []string, targetLang string) ([]cachedTranslation, error) {
	payload, err := json.Marshal(deepLTranslateRequest{Text: texts, TargetLang: targetLang})
	if err != nil {
		return nil, err
	}

	body, err := callDeepL(http.MethodPost, "/translate", payload)
	if err != nil {
		return nil, err
	}

	var parsed deepLTranslateResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("respuesta de DeepL ilegible: %w", err)
	}
	if len(parsed.Translations) != len(texts) {
		return nil, fmt.Errorf("DeepL devolvió %d traducciones para %d textos", len(parsed.Translations), len(texts))
	}

	results := make([]cachedTranslation, len(texts))
	for i, translation := range parsed.Translations {
		results[i] = cachedTranslation{
			text:         translation.Text,
			detectedLang: strings.ToUpper(translation.DetectedSourceLanguage),
		}
	}
	return results, nil
}

func translate(text, targetLang string) (cachedTranslation, error) {
	results, err := translateMany([]string{text}, targetLang)
	if err != nil {
		return cachedTranslation{}, err
	}
	return results[0], nil
}

func cacheKeyFor(text, targetLang string) (string, bool) {
	return targetLang + "|" + strings.ToLower(text), len(text) <= maxCachedTextBytes
}

func translateWithCache(text, targetLang string) (cachedTranslation, error) {
	cacheKey, cacheable := cacheKeyFor(text, targetLang)

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
	words := splitWords(text)

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

// Errores nuestros (DeepL falló, cuota agotada...) se devuelven como 424 y no
// como 5xx: SL bloquea un script que recibe 5 errores 500+ en 60 segundos.
const statusTranslationFailed = http.StatusFailedDependency

// Limpia un mensaje entrante. Devuelve "" si no merece llamar a DeepL
// (risas, enlaces, sin letras o español reconocido localmente).
func prepareIncoming(text string) string {
	text = removeLinks(strings.TrimSpace(text))
	if !needsTranslation(text) || looksSpanish(text) {
		return ""
	}
	return text
}

// Línea a mostrar al dueño, o "" si no hay nada que mostrar porque ya era
// español o la "traducción" sale igual que el original (DeepL se equivocó de idioma).
func formatIncoming(original string, result cachedTranslation) string {
	if result.detectedLang == nativeLanguage || lettersOnly(result.text) == lettersOnly(original) {
		return ""
	}
	line := fmt.Sprintf("[%s] %s", result.detectedLang, result.text)
	return strings.ReplaceAll(line, "\n", " ") // una línea por mensaje (formato del lote)
}

// Mensajes ENTRANTES de uno en uno (lo usan los HUD con el script antiguo).
// 200 con texto -> traducción; 200 vacío o 204 -> no mostrar nada.
func translateInHandler(w http.ResponseWriter, r *http.Request) {
	text, ok := readChatText(w, r)
	if !ok {
		return
	}

	text = prepareIncoming(text)
	if text == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	result, err := translateWithCache(text, nativeLanguage)
	if err != nil {
		log.Printf("[in] %v", err)
		http.Error(w, err.Error(), statusTranslationFailed)
		return
	}

	line := formatIncoming(text, result)
	if line == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writePlainText(w, line)
}

// Mensajes ENTRANTES en lote: una línea por mensaje en la petición y una
// línea por mensaje en la respuesta, en el mismo orden (vacía = no mostrar).
// Todo lo que haga falta traducir va en UNA sola llamada a DeepL.
func translateBatchHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "usa POST", http.StatusMethodNotAllowed)
		return
	}

	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBatchBodyBytes))
	if err != nil {
		http.Error(w, "lote demasiado grande o ilegible", http.StatusBadRequest)
		return
	}

	lines := strings.Split(string(raw), "\n")
	if len(lines) > maxBatchMessages {
		http.Error(w, "demasiados mensajes en el lote", http.StatusBadRequest)
		return
	}

	output := make([]string, len(lines))
	var pendingIndexes []int
	var pendingTexts []string

	for i, line := range lines {
		text := prepareIncoming(line)
		if text == "" {
			continue
		}
		cacheKey, cacheable := cacheKeyFor(text, nativeLanguage)
		if cacheable {
			if entry, found := cache.get(cacheKey); found {
				output[i] = formatIncoming(text, entry)
				continue
			}
		}
		pendingIndexes = append(pendingIndexes, i)
		pendingTexts = append(pendingTexts, text)
	}

	if len(pendingTexts) > 0 {
		results, err := translateMany(pendingTexts, nativeLanguage)
		if err != nil {
			log.Printf("[batch] %v", err)
			http.Error(w, err.Error(), statusTranslationFailed)
			return
		}
		for n, index := range pendingIndexes {
			if cacheKey, cacheable := cacheKeyFor(pendingTexts[n], nativeLanguage); cacheable {
				cache.put(cacheKey, results[n])
			}
			output[index] = formatIncoming(pendingTexts[n], results[n])
		}
	}

	writePlainText(w, strings.Join(output, "\n"))
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
		http.Error(w, err.Error(), statusTranslationFailed)
		return
	}

	writePlainText(w, result.text)
}

// Consumo de DeepL en el periodo actual (para /7 !uso).
func usageHandler(w http.ResponseWriter, r *http.Request) {
	body, err := callDeepL(http.MethodGet, "/usage", nil)
	if err != nil {
		log.Printf("[usage] %v", err)
		http.Error(w, err.Error(), statusTranslationFailed)
		return
	}

	var usage deepLUsageResponse
	if err := json.Unmarshal(body, &usage); err != nil {
		http.Error(w, "respuesta de uso ilegible", statusTranslationFailed)
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
	http.HandleFunc("/translate-batch", translateBatchHandler)
	http.HandleFunc("/translate-out", translateOutHandler)

	log.Printf("Servidor traductor escuchando en el puerto %s...", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
