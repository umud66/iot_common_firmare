package main

import (
	"fmt"
	"log"
)

type gpioBackend interface {
	WritePWM(targetID string, gpio int, duty int) error
	WriteRelay(targetID string, gpio int, state string) error
}

type consoleGPIOBackend struct{}

func (consoleGPIOBackend) WritePWM(targetID string, gpio int, duty int) error {
	fmt.Printf("[GPIO] pwm target=%s gpio=%d duty=%d\n", targetID, gpio, duty)
	log.Printf("[GPIO] pwm target=%s gpio=%d duty=%d", targetID, gpio, duty)
	return nil
}

func (consoleGPIOBackend) WriteRelay(targetID string, gpio int, state string) error {
	fmt.Printf("[GPIO] relay target=%s gpio=%d state=%s\n", targetID, gpio, state)
	log.Printf("[GPIO] relay target=%s gpio=%d state=%s", targetID, gpio, state)
	return nil
}
