package main

import "testing"

// A shared contact carries its number whichever shape the sender's phone gives
// the TEL line: parameters after the property name, or a group prefix before it.
func TestParseVcardTelShapes(t *testing.T) {
	cases := []struct {
		name  string
		vcard string
	}{
		{"parameters", "BEGIN:VCARD\r\nVERSION:3.0\r\nN:;Gato Ficardi;;;\r\nFN:Gato Ficardi\r\nTEL;type=CELL;type=VOICE;waid=5492611234567:+54 9 261 123-4567\r\nEND:VCARD\r\n"},
		{"grouped", "BEGIN:VCARD\nVERSION:3.0\nN:Ficardi;Gato;;;\nFN:Gato Ficardi\nitem1.TEL;type=CELL;waid=5492611234567:+54 9 261 123-4567\nitem1.X-ABLabel:Celular\nEND:VCARD\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			contact := parseVcard("", c.vcard)
			if contact.Name.FormattedName != "Gato Ficardi" {
				t.Fatalf("name = %q", contact.Name.FormattedName)
			}
			if len(contact.Phones) != 1 {
				t.Fatalf("phones = %+v", contact.Phones)
			}
			phone := contact.Phones[0]
			if phone.Phone != "+54 9 261 123-4567" || phone.WaID != "5492611234567" || phone.Type != "CELL" {
				t.Fatalf("phone = %+v", phone)
			}
		})
	}
}

// The display name the message carries outranks the card's FN, and a card with
// no TEL line yields a contact with no phones rather than an empty one.
func TestParseVcardDisplayNameAndNoTel(t *testing.T) {
	contact := parseVcard("Gato", "BEGIN:VCARD\nFN:Gato Ficardi\nEND:VCARD\n")
	if contact.Name.FormattedName != "Gato" {
		t.Fatalf("name = %q", contact.Name.FormattedName)
	}
	if contact.Phones != nil {
		t.Fatalf("phones = %+v", contact.Phones)
	}
}
