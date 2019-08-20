package handler

import (
	"bufio"
	"io"
	"strings"
)

func readProperties(propsFile io.Reader) (map[string]string, error) {
	properties := make(map[string]string)
	scanner := bufio.NewScanner(propsFile)
	for scanner.Scan() {
		line := scanner.Text()
		equalIndex := strings.Index(line, "=")
		if equalIndex >= 0 {
			key := strings.TrimSpace(line[:equalIndex])
			if len(key) > 0 {
				value := strings.TrimSpace(line[equalIndex+1:])
				properties[key] = value
			}
		}
	}
	return properties, scanner.Err()
}

func expandProperty(properties map[string]string, name string) string {
	value := properties[name]
	varStart := strings.Index(value, "{")
	for varStart >= 0 {
		varEnd := strings.Index(value[varStart:], "}")
		if varEnd >= 0 {
			referencedName := value[varStart+1 : varStart+varEnd]
			expanded := expandProperty(properties, referencedName)
			value = value[:varStart] + expanded + value[varStart+varEnd+1:]
			varStart = strings.Index(value, "{")
		} else {
			varStart = -1
		}
	}
	return value
}
