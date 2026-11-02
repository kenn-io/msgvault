package cmd

import "go.kenn.io/msgvault/internal/senderclass"

func addSenderClassJSON(output map[string]any, readout senderclass.Readout) {
	if readout.SenderClass != "" {
		output["sender_class"] = readout.SenderClass
	}
	if readout.SenderClassBasis != "" {
		output["sender_class_basis"] = readout.SenderClassBasis
	}
	if readout.SenderRelationship != "" {
		output["sender_relationship"] = readout.SenderRelationship
	}
}
