package product

import (
	"bytes"
	"encoding/json"
	"io"
)

// StrictJSON rejects duplicate properties, excessive nesting, unknown fields,
// null documents, and trailing JSON. It never interprets input as HTML or SQL.
func StrictJSON(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 32 {
			return ErrValidation
		}
		tok, err := dec.Token()
		if err != nil {
			return ErrValidation
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{':
				seen := map[string]bool{}
				for dec.More() {
					key, err := dec.Token()
					if err != nil {
						return ErrValidation
					}
					s, ok := key.(string)
					if !ok || seen[s] {
						return ErrValidation
					}
					seen[s] = true
					if err = visit(depth + 1); err != nil {
						return err
					}
				}
			case '[':
				for dec.More() {
					if err := visit(depth + 1); err != nil {
						return err
					}
				}
			default:
				return ErrValidation
			}
			_, err = dec.Token()
			if err != nil {
				return ErrValidation
			}
		}
		return nil
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return ErrValidation
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return ErrValidation
	}
	strict := json.NewDecoder(bytes.NewReader(data))
	strict.DisallowUnknownFields()
	if err := strict.Decode(dst); err != nil {
		return ErrValidation
	}
	return nil
}
func DecodeInput(data []byte) (Input, error) {
	var in Input
	var fields map[string]json.RawMessage
	if StrictJSON(data, &fields) != nil || StrictJSON(data, &in) != nil {
		return in, ErrValidation
	}
	for _, key := range []string{"name", "description", "category", "priceRwf", "stockQuantity", "availability", "location", "lowStockThreshold", "condition", "specifications"} {
		if v, ok := fields[key]; !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return in, ErrValidation
		}
	}
	return in, nil
}
func Merge(in Input, patch map[string]json.RawMessage) (Input, error) {
	if len(patch) == 0 {
		return Input{}, ErrValidation
	}
	b, _ := json.Marshal(in)
	var merged map[string]json.RawMessage
	_ = json.Unmarshal(b, &merged)
	for k, v := range patch {
		if k == "sku" {
			return Input{}, ErrValidation
		}
		if _, ok := merged[k]; !ok {
			return Input{}, ErrValidation
		}
		if bytes.Equal(bytes.TrimSpace(v), []byte("null")) && !Contains([]string{"brand", "model", "comparisonPriceRwf"}, k) {
			return Input{}, ErrValidation
		}
		merged[k] = v
	}
	b, _ = json.Marshal(merged)
	result, err := DecodeInput(b)
	if err != nil || len(Validate(result)) != 0 {
		return result, ErrValidation
	}
	return result, nil
}
