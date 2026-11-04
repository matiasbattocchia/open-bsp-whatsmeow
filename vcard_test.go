package main

import (
	"encoding/json"
	"strings"
	"testing"
)

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

// A card written from a formatted name alone carries the whole name as N's
// given name, escapes its text values, and keeps every TEL on its own line.
func TestBuildVcardFormattedName(t *testing.T) {
	var contact ContactData
	if err := json.Unmarshal([]byte(`{
		"name": {"formatted_name": "Pérez, Juan; \"El Gordo\""},
		"phones": [
			{"phone": "+54 9 261 725-3489", "wa_id": "5492617253489"},
			{"phone": "0261 4123456\r\nX-INJECTED:1", "wa_id": "54;9\n"}
		]
	}`), &contact); err != nil {
		t.Fatal(err)
	}
	name, vcard := buildVcard(contact)
	if name != `Pérez, Juan; "El Gordo"` {
		t.Fatalf("name = %q", name)
	}
	want := "BEGIN:VCARD\nVERSION:3.0\n" +
		`N:;Pérez\, Juan\; "El Gordo";;;` + "\n" +
		`FN:Pérez\, Juan\; "El Gordo"` + "\n" +
		"TEL;type=CELL;waid=5492617253489:+54 9 261 725-3489\n" +
		"TEL;type=CELL;waid=549:0261 4123456X-INJECTED:1\n" +
		"END:VCARD"
	if vcard != want {
		t.Fatalf("vcard =\n%s\nwant\n%s", vcard, want)
	}
}

// Structured name fields fill N's components, each escaped on its own, and a
// missing formatted name is composed from them.
func TestBuildVcardStructuredName(t *testing.T) {
	var contact ContactData
	contact.Name.FirstName = "Juan"
	contact.Name.LastName = `Pérez\García`
	contact.Name.Prefix = "Dr."
	contact.Name.Suffix = "Jr,\nII"
	name, vcard := buildVcard(contact)
	if name != "Dr. Juan Pérez\\García Jr,\nII" {
		t.Fatalf("name = %q", name)
	}
	if !strings.Contains(vcard, "\n"+`N:Pérez\\García;Juan;;Dr.;Jr\,\nII`+"\n") {
		t.Fatalf("vcard = %q", vcard)
	}
	if !strings.Contains(vcard, "\n"+`FN:Dr. Juan Pérez\\García Jr\,\nII`+"\n") {
		t.Fatalf("vcard = %q", vcard)
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
