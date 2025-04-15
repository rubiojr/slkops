package main

import (
	"bufio"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	usernameStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("5")).Bold(true)
	timeStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	messageStyle  = lipgloss.NewStyle()
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))

	channelStyle = lipgloss.NewStyle().
			Background(lipgloss.Color("62")).
			Foreground(lipgloss.Color("230")).
			Padding(0, 1)

	inputStyle = lipgloss.NewStyle().
			PaddingLeft(1).
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("63"))
)

// Message types
type fetchMessagesMsg struct {
	messages []Message
	err      error
}

type sendMessageMsg struct {
	response *SendMessageResponse
	err      error
}

type tickMsg time.Time

// This is a new message type to explicitly trigger a redraw
type redrawViewportMsg struct{}

type formattedMessage struct {
	text      string
	timestamp time.Time
	id        string // message ID (ts)
}

type model struct {
	client       *SlackClient
	channelID    string
	channelName  string
	messages     []formattedMessage
	messageIDs   map[string]bool
	input        textinput.Model
	viewport     viewport.Model
	err          error
	ready        bool
	lastFetched  string
	history      []string
	historyIndex int
	historyFile  string
	browsingHist bool
	refreshCount int
	needsRedraw  bool // Flag to indicate the viewport needs redrawing
}

func initialModel(client *SlackClient, channelID string) (model, error) {
	// Get channel info to display the name in the UI
	var channelName string
	channel, err := client.ChannelInfo(channelID)
	if err != nil {
		// If we can't get the channel info, just use the ID as the name
		channelName = channelID
	} else {
		channelName = channel.Name
	}

	// Use textinput instead of textarea for single line
	ti := textinput.New()
	ti.Placeholder = "Send a message..."
	ti.Focus()
	ti.Width = 30
	ti.CharLimit = 4000
	ti.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("62"))
	ti.Prompt = "➤ "

	vp := viewport.New(30, 10)
	vp.SetContent("")

	// Get user home directory for history file
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return model{}, err
	}

	// Ensure directory exists
	historyDir := filepath.Join(homeDir, ".slack-chat-history")
	if err := os.MkdirAll(historyDir, 0755); err != nil {
		return model{}, err
	}

	historyFile := filepath.Join(historyDir, fmt.Sprintf("%s-%s.history", client.team, channelID))

	// Load history from file
	history := []string{}
	if _, err := os.Stat(historyFile); err == nil {
		file, err := os.Open(historyFile)
		if err == nil {
			defer file.Close()
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				history = append(history, scanner.Text())
			}
		}
	}

	m := model{
		client:       client,
		channelID:    channelID,
		channelName:  channelName,
		messages:     []formattedMessage{},
		messageIDs:   make(map[string]bool),
		input:        ti,
		viewport:     vp,
		ready:        false,
		history:      history,
		historyIndex: len(history),
		historyFile:  historyFile,
		browsingHist: false,
		refreshCount: 0,
		needsRedraw:  false,
	}

	return m, nil
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		tea.EnterAltScreen,
		fetchMessages(m.client, m.channelID, m.lastFetched),
		textinput.Blink,
		tick(),
	)
}

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// Command to trigger a redraw
func redrawViewport() tea.Cmd {
	return func() tea.Msg {
		return redrawViewportMsg{}
	}
}

func sendMessage(client *SlackClient, channelID, text string) tea.Cmd {
	return func() tea.Msg {
		resp, err := client.SendMessage(channelID, text)
		return sendMessageMsg{resp, err}
	}
}

func (m *model) appendToHistory(message string) error {
	// Don't add empty messages or duplicates of the last message
	if strings.TrimSpace(message) == "" {
		return nil
	}
	if len(m.history) > 0 && m.history[len(m.history)-1] == message {
		return nil
	}

	m.history = append(m.history, message)
	m.historyIndex = len(m.history)

	// Write to file
	file, err := os.OpenFile(m.historyFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.WriteString(message + "\n")
	return err
}

func (m *model) navigateHistory(direction int) {
	newIndex := m.historyIndex + direction

	// Check bounds
	if newIndex < 0 {
		newIndex = 0
	} else if newIndex > len(m.history) {
		newIndex = len(m.history)
	}

	if newIndex != m.historyIndex {
		m.historyIndex = newIndex

		// If at end of history, clear input
		if m.historyIndex == len(m.history) {
			m.input.SetValue("")
		} else if len(m.history) > 0 {
			m.input.SetValue(m.history[m.historyIndex])
		}
		m.browsingHist = m.historyIndex < len(m.history)
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var (
		tiCmd  tea.Cmd
		vpCmd  tea.Cmd
		cmds   []tea.Cmd
		height int
		width  int
	)

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyEsc, tea.KeyCtrlC:
			return m, tea.Quit
		case tea.KeyEnter:
			if strings.TrimSpace(m.input.Value()) != "" {
				text := m.input.Value()
				err := m.appendToHistory(text)
				if err != nil {
					m.err = err
				}

				// Immediately send the message and then fetch updated messages
				cmds = append(cmds, tea.Sequence(
					sendMessage(m.client, m.channelID, text),
					// Increased delay to allow server to process
					tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg {
						return fetchMessagesMsg{nil, nil}
					}),
				))

				m.input.Reset()
				m.browsingHist = false
			}
		case tea.KeyUp:
			m.navigateHistory(-1)
			return m, nil
		case tea.KeyDown:
			m.navigateHistory(1)
			return m, nil
		}

	case tea.WindowSizeMsg:
		height = msg.Height
		width = msg.Width

		if !m.ready {
			m.viewport = viewport.New(width, height-4)
			m.input.Width = width - 4 // Account for prompt and some padding
			m.ready = true
		} else {
			m.viewport.Width = width
			m.viewport.Height = height - 4
			m.input.Width = width - 4 // Account for prompt and some padding
		}
		m.updateViewportContent()

	case redrawViewportMsg:
		// This message just forces a redraw of the viewport
		m.updateViewportContent()

	case tickMsg:
		// Refresh counter
		m.refreshCount++

		// Schedule the next tick and fetch messages
		cmds = append(cmds, tick())
		cmds = append(cmds, fetchMessages(m.client, m.channelID, m.lastFetched))
		return m, tea.Batch(cmds...)

	case fetchMessagesMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}

		// If this is an immediate refresh after sending a message
		if msg.messages == nil {
			return m, fetchMessages(m.client, m.channelID, "")
		}

		if len(msg.messages) > 0 {
			// Track if we've added any messages
			messagesAdded := false

			// Process new messages
			for _, message := range msg.messages {
				// Skip messages we've already processed
				if m.messageIDs[message.Ts] {
					continue
				}

				ts, _ := strconv.ParseFloat(message.Ts, 64)
				timestamp := time.Unix(int64(ts), 0)

				username, err := m.client.UsernameForMessage(message)
				if err != nil {
					username = "unknown"
				}

				formattedText := fmt.Sprintf("%s %s: %s",
					timeStyle.Render(timestamp.Format("15:04:05")),
					usernameStyle.Render(username),
					messageStyle.Render(message.Text),
				)

				m.messages = append(m.messages, formattedMessage{
					text:      formattedText,
					timestamp: timestamp,
					id:        message.Ts,
				})

				m.messageIDs[message.Ts] = true
				messagesAdded = true
			}

			if messagesAdded {
				// Sort messages by timestamp
				sort.Slice(m.messages, func(i, j int) bool {
					return m.messages[i].timestamp.Before(m.messages[j].timestamp)
				})

				// Update the last fetched timestamp
				if len(msg.messages) > 0 {
					m.lastFetched = msg.messages[0].Ts
				}

				// Always update the viewport content when messages change
				m.updateViewportContent()
			}
		}

	case sendMessageMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Force a refresh of messages after sending
		return m, fetchMessages(m.client, m.channelID, "")
	}

	// Always update these components
	m.input, tiCmd = m.input.Update(msg)
	m.viewport, vpCmd = m.viewport.Update(msg)

	// Add in any other commands we've collected
	cmds = append(cmds, tiCmd, vpCmd)

	return m, tea.Batch(cmds...)
}

func (m *model) updateViewportContent() {
	var content strings.Builder
	for _, msg := range m.messages {
		content.WriteString(msg.text + "\n")
	}

	// Show refresh count as a debugging aid
	//content.WriteString(fmt.Sprintf("\n[Refreshed %d times]", m.refreshCount))

	m.viewport.SetContent(content.String())
	m.viewport.GotoBottom()
}

// Modified to be more robust in fetching messages
func fetchMessages(client *SlackClient, channelID, since string) tea.Cmd {
	return func() tea.Msg {
		// If no since timestamp is provided, fetch the most recent messages
		limit := 20
		history, err := client.History(channelID, since, "", limit)
		if err != nil {
			return fetchMessagesMsg{nil, err}
		}

		return fetchMessagesMsg{history.Messages, nil}
	}
}

func (m model) View() string {
	if !m.ready {
		return "Initializing..."
	}

	if m.err != nil {
		return fmt.Sprintf("Error: %s\nPress Ctrl+C to quit.", m.err)
	}

	channelHeader := channelStyle.Render(fmt.Sprintf("#%s", m.channelName))
	messagesView := m.viewport.View()

	inputField := inputStyle.Render(m.input.View())

	historyIndicator := ""
	if m.browsingHist {
		historyIndicator = fmt.Sprintf(" [History: %d/%d]", m.historyIndex+1, len(m.history))
	}

	return fmt.Sprintf("%s\n\n%s\n\n%s%s", channelHeader, messagesView, inputField, historyIndicator)
}

func main() {
	// Use io.Discard for the logger
	logger := log.New(io.Discard, "", 0)

	if len(os.Args) < 3 {
		fmt.Println("Usage: slack-chat <team> <channelID>")
		os.Exit(1)
	}

	team := os.Args[1]
	channelID := os.Args[2]

	client, err := NewClient(team, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating client: %v\n", err)
		os.Exit(1)
	}

	initialModel, err := initialModel(client, channelID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error creating model: %v\n", err)
		os.Exit(1)
	}

	p := tea.NewProgram(initialModel, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running program: %v\n", err)
		os.Exit(1)
	}
}
