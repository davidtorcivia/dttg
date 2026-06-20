package web

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"sync"
	"time"
)

// weatherCache holds the latest NYC temperature, refreshed server-side so the
// header works regardless of the visitor's network. open-meteo, no API key.
type weatherCache struct {
	mu      sync.RWMutex
	tempF   *int
	updated time.Time
}

const openMeteoURL = "https://api.open-meteo.com/v1/forecast?latitude=40.7128&longitude=-74.0060&current_weather=true&temperature_unit=fahrenheit"

func (s *Server) startWeather() {
	go func() {
		s.refreshWeather()
		t := time.NewTicker(15 * time.Minute)
		defer t.Stop()
		for range t.C {
			s.refreshWeather()
		}
	}()
}

func (s *Server) refreshWeather() {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openMeteoURL, nil)
	if err != nil {
		return
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var data struct {
		CurrentWeather struct {
			Temperature float64 `json:"temperature"`
		} `json:"current_weather"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return
	}
	t := int(math.Round(data.CurrentWeather.Temperature))
	s.weather.mu.Lock()
	s.weather.tempF = &t
	s.weather.updated = time.Now()
	s.weather.mu.Unlock()
}

func (s *Server) handleWeather(w http.ResponseWriter, _ *http.Request) {
	s.weather.mu.RLock()
	temp := s.weather.tempF
	s.weather.mu.RUnlock()

	resp := map[string]any{"city": "NYC", "unit": "F"}
	if temp != nil {
		resp["temp"] = *temp
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_ = json.NewEncoder(w).Encode(resp)
}
