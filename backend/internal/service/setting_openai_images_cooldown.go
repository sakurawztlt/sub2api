package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

func (s *SettingService) GetOpenAIImagesOAuthUnavailableCooldownSettings(ctx context.Context) (*OpenAIImagesOAuthUnavailableCooldownSettings, error) {
	value, err := s.settingRepo.GetValue(ctx, SettingKeyOpenAIImagesOAuthUnavailableCooldownSettings)
	if err != nil {
		if errors.Is(err, ErrSettingNotFound) {
			return DefaultOpenAIImagesOAuthUnavailableCooldownSettings(), nil
		}
		return nil, fmt.Errorf("get OpenAI images OAuth unavailable cooldown settings: %w", err)
	}
	if value == "" {
		return DefaultOpenAIImagesOAuthUnavailableCooldownSettings(), nil
	}

	var settings OpenAIImagesOAuthUnavailableCooldownSettings
	if err := json.Unmarshal([]byte(value), &settings); err != nil ||
		settings.CooldownMinutes <= 0 || settings.CooldownMinutes > openAIImagesOAuthUnavailableMaxCooldownMinutes {
		return DefaultOpenAIImagesOAuthUnavailableCooldownSettings(), nil
	}
	return &settings, nil
}

func (s *SettingService) SetOpenAIImagesOAuthUnavailableCooldownSettings(ctx context.Context, settings *OpenAIImagesOAuthUnavailableCooldownSettings) error {
	if settings == nil {
		return fmt.Errorf("settings cannot be nil")
	}
	if settings.CooldownMinutes <= 0 || settings.CooldownMinutes > openAIImagesOAuthUnavailableMaxCooldownMinutes {
		return fmt.Errorf("cooldown_minutes must be between 1-%d", openAIImagesOAuthUnavailableMaxCooldownMinutes)
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal OpenAI images OAuth unavailable cooldown settings: %w", err)
	}
	return s.settingRepo.Set(ctx, SettingKeyOpenAIImagesOAuthUnavailableCooldownSettings, string(data))
}
