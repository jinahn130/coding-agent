// Application state
let state = {
    repositories: [],
    selectedRepository: null,
    chatMessages: [],
    isConnected: false,
    websocket: null,
    sessionId: null
};

// API endpoints
const API_BASE = window.location.origin;
const WS_BASE = `ws://${window.location.host}`;

// Initialize application
document.addEventListener('DOMContentLoaded', function() {
    setupEventListeners();
    loadRepositories();
});

function setupEventListeners() {
    // File upload
    const fileInput = document.getElementById('fileInput');
    const uploadArea = document.getElementById('uploadArea');

    fileInput.addEventListener('change', handleFileUpload);

    // Drag and drop
    uploadArea.addEventListener('dragover', (e) => {
        e.preventDefault();
        uploadArea.classList.add('dragover');
    });

    uploadArea.addEventListener('dragleave', () => {
        uploadArea.classList.remove('dragover');
    });

    uploadArea.addEventListener('drop', (e) => {
        e.preventDefault();
        uploadArea.classList.remove('dragover');
        const files = e.dataTransfer.files;
        if (files.length > 0) {
            uploadFile(files[0]);
        }
    });

    // Chat input
    const messageInput = document.getElementById('messageInput');
    messageInput.addEventListener('keypress', (e) => {
        if (e.key === 'Enter' && !e.shiftKey) {
            e.preventDefault();
            sendMessage();
        }
    });
}

// Repository management
async function loadRepositories() {
    try {
        const response = await fetch(`${API_BASE}/v1/repositories`, {
            headers: {
                'Content-Type': 'application/json'
            }
        });

        if (!response.ok) {
            throw new Error(`HTTP ${response.status}: ${response.statusText}`);
        }

        const data = await response.json();
        state.repositories = data.repositories || [];
        renderRepositories();
    } catch (error) {
        console.error('Failed to load repositories:', error);
        showError('Failed to load repositories: ' + error.message);
    }
}

function renderRepositories() {
    const container = document.getElementById('repositoryList');

    if (state.repositories.length === 0) {
        container.innerHTML = '<div class="empty-state">No repositories uploaded yet</div>';
        return;
    }

    const html = state.repositories.map(repo => {
        const statusClass = `status-${repo.ingestionStatus?.state?.toLowerCase().replace('state_', '') || 'unknown'}`;
        const statusText = getStatusText(repo.ingestionStatus?.state);
        const isSelected = state.selectedRepository?.repositoryId === repo.repositoryId;

        return `
            <div class="repository-item ${isSelected ? 'selected' : ''}"
                 onclick="selectRepository('${repo.repositoryId}')">
                <div class="repository-name">${repo.name}</div>
                <div class="repository-status ${statusClass}">${statusText}</div>
                ${repo.ingestionStatus?.state === 'STATE_PENDING' || repo.ingestionStatus?.state === 'STATE_PROCESSING'
                    ? '<div class="loading"></div>' : ''}
            </div>
        `;
    }).join('');

    container.innerHTML = html;
}

function getStatusText(state) {
    const stateMap = {
        'STATE_PENDING': 'Pending',
        'STATE_EXTRACTING': 'Extracting...',
        'STATE_CHUNKING': 'Chunking...',
        'STATE_EMBEDDING': 'Generating embeddings...',
        'STATE_INDEXING': 'Indexing...',
        'STATE_READY': 'Ready',
        'STATE_FAILED': 'Failed'
    };
    return stateMap[state] || 'Unknown';
}

async function selectRepository(repositoryId) {
    console.log('selectRepository called with:', repositoryId);
    const repo = state.repositories.find(r => r.repositoryId === repositoryId);
    console.log('Found repository:', repo);

    if (!repo) {
        console.error('Repository not found:', repositoryId);
        return;
    }

    console.log('Repository ingestion status:', repo.ingestionStatus);
    if (repo.ingestionStatus?.state !== 'STATE_READY') {
        showError('Repository is not ready yet. Please wait for ingestion to complete.');
        return;
    }

    state.selectedRepository = repo;
    state.chatMessages = [];

    console.log('Repository selected successfully, updating UI...');

    // Update UI
    renderRepositories();
    updateChatTitle();
    renderMessages();
    enableChat();

    // Close existing WebSocket connection
    if (state.websocket) {
        state.websocket.close();
    }

    // Establish new WebSocket connection
    connectWebSocket();
}

function updateChatTitle() {
    const title = document.getElementById('chatTitle');
    if (state.selectedRepository) {
        title.textContent = `Chat with ${state.selectedRepository.name}`;
    } else {
        title.textContent = 'Select a repository to start chatting';
    }
}

function enableChat() {
    const messageInput = document.getElementById('messageInput');
    const sendButton = document.getElementById('sendButton');

    console.log('enableChat called, selectedRepository:', state.selectedRepository);
    console.log('ingestionStatus:', state.selectedRepository?.ingestionStatus);

    if (state.selectedRepository && state.selectedRepository.ingestionStatus?.state === 'STATE_READY') {
        console.log('Enabling chat interface...');
        messageInput.disabled = false;
        sendButton.disabled = false;
        messageInput.placeholder = 'Ask a question about your repository...';
    } else {
        console.log('Disabling chat interface...');
        messageInput.disabled = true;
        sendButton.disabled = true;
        messageInput.placeholder = 'Select a ready repository to start chatting...';
    }
}

// File upload
function handleFileUpload(event) {
    const file = event.target.files[0];
    if (file) {
        uploadFile(file);
    }
}

async function uploadFile(file) {
    const allowedTypes = ['.zip', '.tar', '.tar.gz', '.tgz'];
    const fileExt = getFileExtension(file.name);

    if (!allowedTypes.some(type => file.name.endsWith(type))) {
        showError('Please upload a valid repository archive (.zip, .tar, .tar.gz, .tgz)');
        return;
    }

    showUploadProgress(true);
    updateProgress(0);

    try {
        // Create upload request
        const uploadData = {
            file_upload: {
                filename: file.name,
                chunk: await fileToBase64(file),
                is_final: true
            },
            tenant_id: 'local',
            idempotency_key: generateId(),
            options: {
                skip_binaries: true,
                max_file_size_mb: 100
            }
        };

        const response = await fetch(`${API_BASE}/v1/upload`, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json'
            },
            body: JSON.stringify(uploadData)
        });

        if (!response.ok) {
            throw new Error(`Upload failed: ${response.statusText}`);
        }

        const result = await response.json();

        // Monitor upload progress
        await monitorUpload(result.upload_id);

        // Reload repositories
        await loadRepositories();

        showUploadProgress(false);
        showSuccess('Repository uploaded successfully!');

    } catch (error) {
        console.error('Upload failed:', error);
        showError('Upload failed: ' + error.message);
        showUploadProgress(false);
    }
}

async function uploadGitRepo() {
    const gitUrl = document.getElementById('gitUrl').value.trim();
    if (!gitUrl) {
        showError('Please enter a Git repository URL');
        return;
    }

    showUploadProgress(true);
    updateProgress(0);

    try {
        const uploadData = {
            git_repository: {
                url: gitUrl,
                ref: 'main'
            },
            tenant_id: 'local',
            idempotency_key: generateId(),
            options: {
                skip_binaries: true
            }
        };

        const response = await fetch(`${API_BASE}/v1/upload/git`, {
            method: 'POST',
            headers: {
                'Content-Type': 'application/json'
            },
            body: JSON.stringify(uploadData)
        });

        if (!response.ok) {
            throw new Error(`Upload failed: ${response.statusText}`);
        }

        const result = await response.json();

        // Monitor upload progress
        await monitorUpload(result.upload_id);

        // Reload repositories
        await loadRepositories();

        // Clear input
        document.getElementById('gitUrl').value = '';

        showUploadProgress(false);
        showSuccess('Repository cloned successfully!');

    } catch (error) {
        console.error('Git clone failed:', error);
        showError('Git clone failed: ' + error.message);
        showUploadProgress(false);
    }
}

async function monitorUpload(uploadId) {
    let attempts = 0;
    const maxAttempts = 60; // 5 minutes with 5-second intervals

    while (attempts < maxAttempts) {
        try {
            const response = await fetch(`${API_BASE}/v1/upload/${uploadId}/status`);
            if (!response.ok) break;

            const status = await response.json();

            if (status.progress) {
                updateProgress(status.progress.progress_percent);
            }

            if (status.status?.state === 'STATE_READY') {
                updateProgress(100);
                break;
            }

            if (status.status?.state === 'STATE_FAILED') {
                throw new Error(status.error_message || 'Upload failed');
            }

            await sleep(5000); // Wait 5 seconds
            attempts++;

        } catch (error) {
            console.error('Error monitoring upload:', error);
            break;
        }
    }
}

// Chat functionality
function connectWebSocket() {
    if (!state.selectedRepository) return;

    const wsUrl = `${WS_BASE}/v1/chat/${state.selectedRepository.repositoryId}/stream`;
    state.websocket = new WebSocket(wsUrl);

    state.websocket.onopen = () => {
        console.log('WebSocket connected');
        state.isConnected = true;

        // Send start message
        const startMessage = {
            start: {
                repository_id: state.selectedRepository.repositoryId,
                tenant_id: 'local',
                options: {
                    max_results: 10,
                    stream_tokens: true
                }
            }
        };

        state.websocket.send(JSON.stringify(startMessage));
    };

    state.websocket.onmessage = (event) => {
        try {
            const data = JSON.parse(event.data);
            handleChatMessage(data);
        } catch (error) {
            console.error('Failed to parse WebSocket message:', error);
        }
    };

    state.websocket.onclose = () => {
        console.log('WebSocket disconnected');
        state.isConnected = false;
    };

    state.websocket.onerror = (error) => {
        console.error('WebSocket error:', error);
        showError('Connection error occurred');
    };
}

function sendMessage() {
    const messageInput = document.getElementById('messageInput');
    const message = messageInput.value.trim();

    if (!message || !state.isConnected || !state.selectedRepository) {
        return;
    }

    // Add user message to chat
    addMessage('user', message);
    messageInput.value = '';

    // Send chat message via WebSocket
    const chatMessage = {
        chat_message: {
            query: message,
            session_id: state.sessionId || generateId()
        }
    };

    state.websocket.send(JSON.stringify(chatMessage));

    // Add system message indicating search
    addMessage('system', 'Searching repository...');
}

function handleChatMessage(data) {
    if (data.search_started) {
        state.sessionId = data.search_started.session_id;
        removeLastSystemMessage();
        addMessage('system', 'Search started...');
    }

    if (data.search_hit) {
        if (data.search_hit.phase === 'HIT_PHASE_EARLY') {
            removeLastSystemMessage();
            addMessage('system', 'Found relevant code, analyzing...');
        }
        // Store search hits for potential display
    }

    if (data.composition_started) {
        removeLastSystemMessage();
        addMessage('system', 'Generating response...');
        // Add empty assistant message that we'll update
        addMessage('assistant', '');
    }

    if (data.composition_token) {
        // Update the last assistant message with new token
        appendToLastMessage(data.composition_token.text);
    }

    if (data.composition_complete) {
        removeLastSystemMessage();
        // If we weren't streaming tokens, add the complete response
        if (!getLastMessage() || getLastMessage().type !== 'assistant') {
            addMessage('assistant', data.composition_complete.full_response);
        }
    }

    if (data.complete) {
        // Chat interaction is complete
        console.log('Chat complete:', data.complete);
    }

    if (data.error) {
        removeLastSystemMessage();
        showError('Chat error: ' + data.error.error_message);
    }
}

function addMessage(type, content) {
    const message = {
        type,
        content,
        timestamp: new Date()
    };

    state.chatMessages.push(message);
    renderMessages();
    scrollToBottom();
}

function appendToLastMessage(text) {
    if (state.chatMessages.length > 0) {
        const lastMessage = state.chatMessages[state.chatMessages.length - 1];
        if (lastMessage.type === 'assistant') {
            lastMessage.content += text;
            renderMessages();
            scrollToBottom();
        }
    }
}

function getLastMessage() {
    return state.chatMessages[state.chatMessages.length - 1];
}

function removeLastSystemMessage() {
    if (state.chatMessages.length > 0) {
        const lastMessage = state.chatMessages[state.chatMessages.length - 1];
        if (lastMessage.type === 'system') {
            state.chatMessages.pop();
            renderMessages();
        }
    }
}

function renderMessages() {
    const container = document.getElementById('chatMessages');

    if (state.chatMessages.length === 0) {
        container.innerHTML = `
            <div class="empty-state">
                ${state.selectedRepository
                    ? 'Start by asking a question about your repository'
                    : 'Upload a repository and select it to start asking questions about your code.'
                }
            </div>
        `;
        return;
    }

    const html = state.chatMessages.map(message => {
        const timeStr = message.timestamp.toLocaleTimeString();
        return `
            <div class="message ${message.type}">
                <div class="message-content">${formatMessageContent(message.content)}</div>
                <div style="font-size: 0.8em; opacity: 0.6; margin-top: 0.5rem;">${timeStr}</div>
            </div>
        `;
    }).join('');

    container.innerHTML = html;
}

function formatMessageContent(content) {
    // Basic markdown-like formatting
    return content
        .replace(/```(\w+)?\n([\s\S]*?)```/g, '<div class="code-chunk"><div class="chunk-header">Code</div><pre>$2</pre></div>')
        .replace(/`([^`]+)`/g, '<code>$1</code>')
        .replace(/\*\*(.*?)\*\*/g, '<strong>$1</strong>')
        .replace(/\*(.*?)\*/g, '<em>$1</em>')
        .replace(/\n/g, '<br>');
}

function scrollToBottom() {
    const chatMessages = document.getElementById('chatMessages');
    chatMessages.scrollTop = chatMessages.scrollHeight;
}

// Utility functions
function showUploadProgress(show) {
    const progressDiv = document.getElementById('uploadProgress');
    progressDiv.style.display = show ? 'block' : 'none';
}

function updateProgress(percent) {
    const progressFill = document.getElementById('progressFill');
    progressFill.style.width = `${percent}%`;
}

function showError(message) {
    // Create error element
    const errorDiv = document.createElement('div');
    errorDiv.className = 'error';
    errorDiv.textContent = message;

    // Insert at top of container
    const container = document.querySelector('.container');
    container.insertBefore(errorDiv, container.firstChild);

    // Remove after 5 seconds
    setTimeout(() => {
        errorDiv.remove();
    }, 5000);
}

function showSuccess(message) {
    // Create success element
    const successDiv = document.createElement('div');
    successDiv.className = 'error';
    successDiv.style.background = '#d4edda';
    successDiv.style.color = '#155724';
    successDiv.style.borderColor = '#c3e6cb';
    successDiv.textContent = message;

    // Insert at top of container
    const container = document.querySelector('.container');
    container.insertBefore(successDiv, container.firstChild);

    // Remove after 3 seconds
    setTimeout(() => {
        successDiv.remove();
    }, 3000);
}

async function fileToBase64(file) {
    return new Promise((resolve, reject) => {
        const reader = new FileReader();
        reader.readAsArrayBuffer(file);
        reader.onload = () => {
            const arrayBuffer = reader.result;
            const bytes = new Uint8Array(arrayBuffer);
            resolve(Array.from(bytes));
        };
        reader.onerror = reject;
    });
}

function getFileExtension(filename) {
    return filename.slice(filename.lastIndexOf('.'));
}

function generateId() {
    return Date.now().toString(36) + Math.random().toString(36).substr(2);
}

function sleep(ms) {
    return new Promise(resolve => setTimeout(resolve, ms));
}

// Refresh repositories periodically
setInterval(loadRepositories, 30000); // Every 30 seconds