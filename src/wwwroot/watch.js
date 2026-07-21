(() => {
  const body = document.body;
  const channelId = body.dataset.channelId;
  const thumbnailUrl = `/thumbnail/${channelId}`;

  const video = document.querySelector('video');
  const overlay = document.getElementById('overlay');
  const overlayContent = overlay.querySelector('.overlay-content');

  let peerConnection = null;
  let hasThumbnail = body.dataset.hasThumbnail === 'true';

  // Whether we currently want to be playing (the stream is believed live).
  // Guards the retry logic so a stream that ends mid-retry stays stopped.
  let desiredLive = false;
  // Retry bookkeeping for the go-live race: the server announces a stream
  // live before its media tracks are registered, so an early WHEP connect
  // can come back with nothing to receive. We retry until media arrives.
  let connectAttempts = 0;
  let mediaWatchdog = null;
  const RETRY_DELAY_MS = 1500;
  const MAX_CONNECT_ATTEMPTS = 15;
  const MEDIA_TIMEOUT_MS = 5000;

  function clearMediaWatchdog() {
    if (mediaWatchdog) {
      clearTimeout(mediaWatchdog);
      mediaWatchdog = null;
    }
  }

  // Renders the overlay for a given UI state. When `showPoster` is true the
  // most recent thumbnail (if any) is shown dimmed behind the content.
  function setState(state, { showPoster = false } = {}) {
    overlay.dataset.state = state;
    overlay.style.backgroundImage =
      showPoster && hasThumbnail ? `url("${thumbnailUrl}")` : 'none';
    overlay.hidden = state === 'playing';
  }

  function renderMessage(text) {
    overlayContent.innerHTML = '';
    const p = document.createElement('p');
    p.className = 'overlay-message';
    p.textContent = text;
    overlayContent.appendChild(p);
  }

  function renderSpinner(text) {
    overlayContent.innerHTML = '';
    const spinner = document.createElement('div');
    spinner.className = 'spinner';
    overlayContent.appendChild(spinner);
    const p = document.createElement('p');
    p.className = 'overlay-message';
    p.textContent = text;
    overlayContent.appendChild(p);
  }

  function renderButton(label, onClick, iconId) {
    overlayContent.innerHTML = '';
    const button = document.createElement('button');
    button.className = 'overlay-button';
    if (iconId) {
      const NS = 'http://www.w3.org/2000/svg';
      const svg = document.createElementNS(NS, 'svg');
      svg.setAttribute('class', 'overlay-button-icon');
      svg.setAttribute('aria-hidden', 'true');
      const use = document.createElementNS(NS, 'use');
      use.setAttribute('href', `#${iconId}`);
      svg.appendChild(use);
      button.appendChild(svg);
    }
    const span = document.createElement('span');
    span.textContent = label;
    button.appendChild(span);
    button.addEventListener('click', onClick);
    overlayContent.appendChild(button);
  }

  function showOffline(message) {
    setState('offline', { showPoster: true });
    renderMessage(message);
  }

  function showConnecting() {
    setState('connecting', { showPoster: true });
    renderSpinner('Connecting…');
  }

  function showTapForSound() {
    setState('playing-muted');
    renderButton('Unmute', async () => {
      video.muted = false;
      try {
        await video.play();
      } catch (err) {
        // Ignore; user can use native controls.
      }
      showPlaying();
    }, 'icon-muted');
  }

  function showPlaying() {
    video.controls = true;
    setState('playing');
  }

  function showPlayBlocked() {
    setState('play-blocked', { showPoster: true });
    renderButton('▶ Play', async () => {
      video.muted = false;
      try {
        await video.play();
        showPlaying();
      } catch (err) {
        // Fall back to muted playback if sound is still blocked.
        video.muted = true;
        try {
          await video.play();
          showTapForSound();
        } catch (err2) {
          showPlayBlocked();
        }
      }
    });
  }

  // Attempts muted autoplay once media is flowing. Muted playback is allowed
  // by browsers without a user gesture, so the video appears immediately.
  async function tryAutoplayMuted(pc) {
    video.muted = true;
    try {
      await video.play();
      if (peerConnection === pc) {
        showTapForSound();
      }
    } catch (err) {
      if (peerConnection === pc) {
        showPlayBlocked();
      }
    }
  }

  // Begin (or restart) playback of a stream we believe to be live.
  function goLive() {
    desiredLive = true;
    connectAttempts = 0;
    startPlayback();
  }

  // Tear down playback and show an offline/ended overlay. This is the
  // desired-not-live state, so pending retries are cancelled.
  function goOffline(message) {
    desiredLive = false;
    stopPlayback();
    showOffline(message);
  }

  // Schedule another WHEP connection attempt after the go-live race, unless
  // the stream has since ended or we've exhausted our attempts.
  function scheduleRetry(pc) {
    if (peerConnection !== pc) {
      return; // superseded by a newer connection
    }
    stopPlayback();
    if (!desiredLive) {
      return;
    }
    if (connectAttempts >= MAX_CONNECT_ATTEMPTS) {
      showOffline('Trouble connecting to the stream — please refresh.');
      return;
    }
    setTimeout(() => {
      if (desiredLive && !peerConnection) {
        startPlayback();
      }
    }, RETRY_DELAY_MS);
  }

  function startPlayback() {
    if (peerConnection) {
      return;
    }
    showConnecting();
    connectAttempts += 1;

    const pc = new RTCPeerConnection();
    peerConnection = pc;
    pc.addTransceiver('video', { direction: 'recvonly' });
    pc.addTransceiver('audio', { direction: 'recvonly' });

    let inboundStream = null;
    let mediaLive = false;

    // Called only once real media actually starts flowing (see below).
    const onMediaLive = () => {
      if (mediaLive || peerConnection !== pc) {
        return;
      }
      mediaLive = true;
      clearMediaWatchdog();
      connectAttempts = 0;
      tryAutoplayMuted(pc);
    };

    pc.ontrack = (event) => {
      if (!inboundStream) {
        inboundStream = new MediaStream();
        video.srcObject = inboundStream;
      }
      inboundStream.addTrack(event.track);
      // A negotiated inbound track starts `muted` and fires `unmute` only
      // once RTP actually arrives. When the go-live announcement beats the
      // streamer's tracks being registered, the server negotiates a dead
      // track that never unmutes — so we wait for real media rather than
      // mere negotiation before considering playback started.
      if (!event.track.muted) {
        onMediaLive();
      } else {
        event.track.addEventListener('unmute', onMediaLive, { once: true });
      }
    };

    // If real media never arrives (the go-live race, or a stalled
    // connection), tear down and retry.
    clearMediaWatchdog();
    mediaWatchdog = setTimeout(() => {
      if (peerConnection === pc && !mediaLive) {
        scheduleRetry(pc);
      }
    }, MEDIA_TIMEOUT_MS);

    pc.createOffer()
      .then((offer) => {
        pc.setLocalDescription(offer);
        return fetch(`/whep/${channelId}`, {
          method: 'POST',
          body: offer.sdp,
          headers: {
            Authorization: 'Bearer none',
            'Content-Type': 'application/sdp',
          },
        });
      })
      .then((r) => {
        if (!r.ok) {
          throw new Error(`WHEP request failed: ${r.status}`);
        }
        return r.text();
      })
      .then((answer) => {
        // The connection may have been torn down while awaiting the answer.
        if (peerConnection !== pc) {
          return;
        }
        return pc.setRemoteDescription({ sdp: answer, type: 'answer' });
      })
      .catch(() => {
        // Request failed outright (e.g. the stream isn't live). Retry if we
        // still expect it to be live, otherwise fall back to offline.
        if (peerConnection !== pc) {
          return;
        }
        if (desiredLive) {
          scheduleRetry(pc);
        } else {
          goOffline('Stream is offline.');
        }
      });
  }

  function stopPlayback() {
    clearMediaWatchdog();
    if (peerConnection) {
      peerConnection.close();
      peerConnection = null;
    }
    video.srcObject = null;
    video.controls = false;
  }

  // --- Real-time stream status over the websocket -----------------------

  let reconnectDelay = 1000;

  function connectStatusSocket() {
    const scheme = location.protocol === 'https:' ? 'wss:' : 'ws:';
    const socket = new WebSocket(`${scheme}//${location.host}/ws`, 'stream-updates');

    socket.addEventListener('open', () => {
      reconnectDelay = 1000;
    });

    socket.addEventListener('message', (event) => {
      let notification;
      try {
        notification = JSON.parse(event.data);
      } catch (err) {
        return;
      }
      if (notification.Id !== channelId) {
        return;
      }
      if (notification.IsLive) {
        if (!desiredLive) {
          goLive();
        }
      } else {
        hasThumbnail = false;
        goOffline('Stream has ended.');
      }
    });

    socket.addEventListener('close', () => {
      // Reconnect with a capped backoff so a tab left open keeps catching
      // future go-live events.
      setTimeout(connectStatusSocket, reconnectDelay);
      reconnectDelay = Math.min(reconnectDelay * 2, 15000);
    });
  }

  // --- Initial state ----------------------------------------------------

  if (body.dataset.live === 'true') {
    goLive();
  } else {
    goOffline('Stream is offline.');
  }
  connectStatusSocket();
})();
