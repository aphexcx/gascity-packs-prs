package main

import "time"

const heldOfferInterval = time.Minute

func (receipt deliveryReceipt) heldAnswer(gated bool, err error) bool {
	return false
}
