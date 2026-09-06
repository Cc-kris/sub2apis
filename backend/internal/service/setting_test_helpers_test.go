package service

import "context"

// settingUpdateRepoStub is shared by setting, gateway, channel, group, and
// Seedace tests.  It deliberately has no build constraint because several
// ordinary test files exercise the runtime rollback switch.
type settingUpdateRepoStub struct {
	updates map[string]string
	values  map[string]string
	getErr  error
}

func (s *settingUpdateRepoStub) Get(ctx context.Context, key string) (*Setting, error) {
	panic("unexpected Get call")
}

func (s *settingUpdateRepoStub) GetValue(ctx context.Context, key string) (string, error) {
	if s.getErr != nil {
		return "", s.getErr
	}
	if value, ok := s.values[key]; ok {
		return value, nil
	}
	if key == SettingKeySalesPricingVersion {
		return string(SalesPricingVersionLegacy), nil
	}
	return "", ErrSettingNotFound
}

func (s *settingUpdateRepoStub) Set(ctx context.Context, key, value string) error {
	panic("unexpected Set call")
}

func (s *settingUpdateRepoStub) GetMultiple(ctx context.Context, keys []string) (map[string]string, error) {
	panic("unexpected GetMultiple call")
}

func (s *settingUpdateRepoStub) SetMultiple(ctx context.Context, settings map[string]string) error {
	s.updates = make(map[string]string, len(settings))
	for key, value := range settings {
		s.updates[key] = value
		if s.values == nil {
			s.values = make(map[string]string)
		}
		s.values[key] = value
	}
	return nil
}

func (s *settingUpdateRepoStub) GetAll(ctx context.Context) (map[string]string, error) {
	panic("unexpected GetAll call")
}

func (s *settingUpdateRepoStub) Delete(ctx context.Context, key string) error {
	panic("unexpected Delete call")
}
