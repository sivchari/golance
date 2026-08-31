// Package speakeruse references speaker.Speaker as a type and calls Speak
// both through an interface-typed value and through a concretely-typed
// value, giving Implementation distinct cursor positions to resolve.
package speakeruse

import (
	"example.com/xrefmod/speaker"
	"example.com/xrefmod/speakerimpl"
)

// S is typed as speaker.Speaker without holding a concrete value.
var S speaker.Speaker

// CallInterface calls Speak through S's interface-typed static type.
func CallInterface() string {
	return S.Speak()
}

// CallConcrete calls Speak through a concretely-typed value.
func CallConcrete() string {
	return speakerimpl.ValSpeaker{}.Speak()
}
