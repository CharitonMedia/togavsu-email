// Package outlookaddin holds the Outlook add-in's files, served by the module at
// /m/email/outlook-addin/. manifest.xml is a template: the module fills in the firm's
// public address from the request host when the admin downloads it.
package outlookaddin

import "embed"

// Files are the add-in's static files.
//
//go:embed manifest.xml taskpane.html taskpane.js connected.html commands.html icon-16.png icon-32.png icon-80.png
var Files embed.FS
