package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"time"
	"unicode/utf8"
)

// validateSchemaValue enforces the subset of JSON Schema 2020-12 emitted by
// this package. Keeping validation beside the schema generator prevents the
// compact runtime discovery representation from becoming a weaker execution
// boundary.
func validateSchemaValue(path string, value any, definition map[string]any) error {
	if raw, exists := definition["oneOf"]; exists {
		branches, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s has an invalid oneOf contract", path)
		}
		matched := 0
		for _, branch := range branches {
			branchDefinition, ok := branch.(map[string]any)
			if ok && validateSchemaValue(path, value, branchDefinition) == nil {
				matched++
			}
		}
		if matched != 1 {
			return fmt.Errorf("%s must match exactly one allowed shape", path)
		}
	}
	if raw, exists := definition["anyOf"]; exists {
		branches, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s has an invalid anyOf contract", path)
		}
		matched := false
		for _, branch := range branches {
			branchDefinition, ok := branch.(map[string]any)
			if ok && validateSchemaValue(path, value, branchDefinition) == nil {
				matched = true
				break
			}
		}
		if !matched {
			return fmt.Errorf("%s must match at least one allowed shape", path)
		}
	}

	if expected, exists := definition["type"]; exists && !matchesSchemaType(value, expected) {
		return fmt.Errorf("%s must be %s", path, schemaTypeDescription(expected))
	}
	if expected, exists := definition["const"]; exists && !schemaValuesEqual(value, expected) {
		return fmt.Errorf("%s must equal %v", path, expected)
	}
	if allowed, exists := definition["enum"]; exists && !matchesEnum(value, allowed) {
		return fmt.Errorf("%s is not an allowed value", path)
	}

	if text, ok := value.(string); ok {
		length := int64(utf8.RuneCountInString(text))
		if minimum, exists := schemaInt64(definition["minLength"]); exists && length < minimum {
			return fmt.Errorf("%s is shorter than %d characters", path, minimum)
		}
		if maximum, exists := schemaInt64(definition["maxLength"]); exists && length > maximum {
			return fmt.Errorf("%s is longer than %d characters", path, maximum)
		}
		if pattern, ok := definition["pattern"].(string); ok {
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("%s has an invalid pattern contract", path)
			}
			if !compiled.MatchString(text) {
				return fmt.Errorf("%s does not match the required pattern", path)
			}
		}
		if format, ok := definition["format"].(string); ok {
			switch format {
			case "date-time":
				if _, err := time.Parse(time.RFC3339, text); err != nil {
					return fmt.Errorf("%s must be an RFC3339 date-time", path)
				}
			case "uri":
				parsed, err := url.Parse(text)
				if err != nil || parsed.Scheme == "" {
					return fmt.Errorf("%s must be an absolute URI", path)
				}
			}
		}
	}

	if number, ok := schemaNumber(value); ok {
		if minimum, exists := schemaFloat64(definition["minimum"]); exists && number < minimum {
			return fmt.Errorf("%s must be at least %v", path, minimum)
		}
		if maximum, exists := schemaFloat64(definition["maximum"]); exists && number > maximum {
			return fmt.Errorf("%s must be at most %v", path, maximum)
		}
	}

	if object, ok := schemaObject(value); ok {
		properties, _ := definition["properties"].(map[string]any)
		for _, required := range schemaStrings(definition["required"]) {
			if _, exists := object[required]; !exists {
				return fmt.Errorf("%s.%s is required", path, required)
			}
		}
		for key, item := range object {
			if property, exists := properties[key]; exists {
				propertyDefinition, ok := property.(map[string]any)
				if !ok {
					return fmt.Errorf("%s.%s has an invalid schema contract", path, key)
				}
				if err := validateSchemaValue(path+"."+key, item, propertyDefinition); err != nil {
					return err
				}
				continue
			}
			switch additional := definition["additionalProperties"].(type) {
			case bool:
				if !additional {
					return fmt.Errorf("%s.%s is not allowed", path, key)
				}
			case map[string]any:
				if err := validateSchemaValue(path+"."+key, item, additional); err != nil {
					return err
				}
			}
		}
	}

	if items, ok := schemaArray(value); ok {
		length := int64(len(items))
		if minimum, exists := schemaInt64(definition["minItems"]); exists && length < minimum {
			return fmt.Errorf("%s must contain at least %d items", path, minimum)
		}
		if maximum, exists := schemaInt64(definition["maxItems"]); exists && length > maximum {
			return fmt.Errorf("%s must contain at most %d items", path, maximum)
		}
		if itemDefinition, ok := definition["items"].(map[string]any); ok {
			for index, item := range items {
				if err := validateSchemaValue(fmt.Sprintf("%s[%d]", path, index), item, itemDefinition); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func matchesSchemaType(value, expected any) bool {
	switch typed := expected.(type) {
	case string:
		return matchesSingleSchemaType(value, typed)
	case []string:
		for _, item := range typed {
			if matchesSingleSchemaType(value, item) {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if name, ok := item.(string); ok && matchesSingleSchemaType(value, name) {
				return true
			}
		}
	}
	return false
}

func matchesSingleSchemaType(value any, expected string) bool {
	switch expected {
	case "null":
		return value == nil
	case "string":
		_, ok := value.(string)
		return ok
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "number":
		_, ok := schemaNumber(value)
		return ok
	case "integer":
		number, ok := schemaNumber(value)
		return ok && !math.IsNaN(number) && !math.IsInf(number, 0) && math.Trunc(number) == number
	case "object":
		_, ok := schemaObject(value)
		return ok
	case "array":
		_, ok := schemaArray(value)
		return ok
	}
	return false
}

func schemaNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	}
	return 0, false
}

func schemaObject(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			converted[key] = item
		}
		return converted, true
	}
	return nil, false
}

func schemaArray(value any) ([]any, bool) {
	if direct, ok := value.([]any); ok {
		return direct, true
	}
	reflected := reflect.ValueOf(value)
	if !reflected.IsValid() || (reflected.Kind() != reflect.Array && reflected.Kind() != reflect.Slice) {
		return nil, false
	}
	items := make([]any, reflected.Len())
	for index := range items {
		items[index] = reflected.Index(index).Interface()
	}
	return items, true
}

func matchesEnum(value, allowed any) bool {
	reflected := reflect.ValueOf(allowed)
	if !reflected.IsValid() || (reflected.Kind() != reflect.Array && reflected.Kind() != reflect.Slice) {
		return false
	}
	for index := 0; index < reflected.Len(); index++ {
		if schemaValuesEqual(value, reflected.Index(index).Interface()) {
			return true
		}
	}
	return false
}

func schemaValuesEqual(left, right any) bool {
	leftNumber, leftIsNumber := schemaNumber(left)
	rightNumber, rightIsNumber := schemaNumber(right)
	if leftIsNumber && rightIsNumber {
		return leftNumber == rightNumber
	}
	return reflect.DeepEqual(left, right)
}

func schemaStrings(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			if text, ok := item.(string); ok {
				items = append(items, text)
			}
		}
		return items
	}
	return nil
}

func schemaInt64(value any) (int64, bool) {
	number, ok := schemaNumber(value)
	if !ok || math.Trunc(number) != number || number < math.MinInt64 || number > math.MaxInt64 {
		return 0, false
	}
	return int64(number), true
}

func schemaFloat64(value any) (float64, bool) {
	return schemaNumber(value)
}

func schemaTypeDescription(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	default:
		return strconv.Quote(fmt.Sprint(typed))
	}
}
