package whatsapp

import "testing"

// The default used to be DEBUG, which prints every protocol frame —
// keepalives included, every twenty-five seconds, forever. It buried every
// other line in the log of a long-lived deployment, and made a real incident
// much harder to diagnose. Debugging the protocol has to be asked for.
func TestOptions_DefaultLogLevelIsNotDebug(t *testing.T) {
	opts := NewOptions()

	if opts.LogLevel == "DEBUG" {
		t.Fatal("le niveau par défaut ne doit pas être DEBUG")
	}
	if opts.LogLevel != "INFO" {
		t.Errorf("niveau par défaut = %q, attendu INFO", opts.LogLevel)
	}
}

func TestOptions_LogLevelIsConfigurable(t *testing.T) {
	if opts := NewOptions(WithLogLevel("DEBUG")); opts.LogLevel != "DEBUG" {
		t.Errorf("niveau = %q, attendu DEBUG", opts.LogLevel)
	}

	// Une valeur vide retombe sur INFO, jamais sur une chaîne que whatsmeow
	// ne saurait pas interpréter.
	provider := &Provider{opts: NewOptions(WithLogLevel(""))}
	if got := provider.logLevel(); got != "INFO" {
		t.Errorf("logLevel() = %q, attendu INFO", got)
	}
}
