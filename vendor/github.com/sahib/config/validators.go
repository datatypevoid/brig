package config

import (
	"fmt"
	"reflect"
	"time"

	e "github.com/pkg/errors"
)

// EnumValidator checks if the supplied string value is in the `options` list.
func EnumValidator(options ...string) func(val interface{}) error {
	return func(val interface{}) error {
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("enum value is not a string: %v", val)
		}

		for _, option := range options {
			if option == s {
				return nil
			}
		}

		return fmt.Errorf("not a valid enum value: %v (allowed: %v)", s, options)
	}
}

// IntRangeValidator checks if the supplied integer value lies in the
// inclusive boundaries of `min` and `max`.
func IntRangeValidator(min, max int64) func(val interface{}) error {
	return func(val interface{}) error {
		i, ok := val.(int64)
		if !ok {
			return fmt.Errorf("value is not an int64: %v", val)
		}

		if i < min {
			return fmt.Errorf("value may not be less than %d", min)
		}

		if i > max {
			return fmt.Errorf("value may not be more than %d", max)
		}

		return nil
	}
}

// FloatRangeValidator checks if the supplied float value lies in the
// inclusive boundaries of `min` and `max`.
func FloatRangeValidator(min, max float64) func(val interface{}) error {
	return func(val interface{}) error {
		i, ok := val.(float64)
		if !ok {
			return fmt.Errorf("value is not a float64: %v", val)
		}

		if i > max {
			return fmt.Errorf("value may not be more than %f", max)
		}

		if i < min {
			return fmt.Errorf("value may not be less than %f", min)
		}

		return nil
	}
}

// DurationValidator asserts that the config value is a valid duration
// that can be parsed by time.ParseDuration.
func DurationValidator() func(val interface{}) error {
	return func(val interface{}) error {
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("value is not a duration string: %v", val)
		}

		_, err := time.ParseDuration(s)
		return err
	}
}

// ListValidator takes any other validator and applies it to a list value.
// If `fn` is nil it only checks if the value is indeed a list.
func ListValidator(fn func(val interface{}) error) func(val interface{}) error {
	return func(val interface{}) error {
		typ := reflect.TypeOf(val)
		if typ.Kind() != reflect.Slice {
			return fmt.Errorf("%v (%T) is not a list", val, val)
		}

		if fn != nil {
			rval := reflect.ValueOf(val)
			for idx := 0; idx < rval.Len(); idx++ {
				if err := fn(rval.Index(idx).Interface()); err != nil {
					return e.Wrapf(err, "elem at index %d", idx)
				}

			}
		}

		return nil
	}
}
