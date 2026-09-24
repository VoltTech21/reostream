// Every player on either page is built here, so "live" means the same
// thing everywhere.
//
// Left alone, mpegts.js treats a live stream like a recording: pause for a
// minute and play again and it resumes a minute behind, and stays there.
// For a camera that is not playback, it is a clock that is wrong. The
// latency chasing options below keep it at the edge, and seeking to the
// end of the buffer on play covers the case where it has already drifted
// before those take effect.
window.reostreamPlayer = function (url, video) {
  var player = mpegts.createPlayer({
    type: 'mpegts',
    isLive: true,
    url: url,
    liveBufferLatencyChasing: true,
    liveBufferLatencyMaxLatency: 3.0,
    liveBufferLatencyMinRemain: 0.5
  });
  if (video) {
    video.addEventListener('play', function () {
      try {
        var b = video.buffered;
        if (b && b.length) {
          var edge = b.end(b.length - 1);
          if (edge - video.currentTime > 1) video.currentTime = edge - 0.1;
        }
      } catch (e) {}
    });
  }
  return player;
};


// Snapshots. The tile's <img> starts empty because the keyframe endpoint
// serves a raw elementary stream no browser can render; the decoder that
// can is the one already on this page, so a frame is decoded here and
// drawn into the tag.
//
// Strictly one at a time, and the connection is dropped the moment a frame
// has been drawn. Eight tiles opening eight streams at once is exactly what
// the play button was built to avoid, and a snapshot must not reintroduce
// it by the back door.
window.reostreamSnapshots = function (containers) {
  if (!window.mpegts || !mpegts.isSupported()) return;
  var queue = Array.prototype.slice.call(containers).filter(function (c) {
    return c.dataset.src && c.querySelector('img') && c.querySelector('video');
  });

  function next() {
    var container = queue.shift();
    if (!container) return;
    var video = container.querySelector('video');
    var img = container.querySelector('img');
    var player = reostreamPlayer(container.dataset.src, video);
    var done = false;

    function finish(drew) {
      if (done) return;
      done = true;
      clearTimeout(timer);
      video.removeEventListener('loadeddata', draw);
      try { player.pause(); } catch (e) {}
      try { player.unload(); } catch (e) {}
      try { player.detachMediaElement(); } catch (e) {}
      try { player.destroy(); } catch (e) {}
      video.hidden = true;
      if (drew) {
        img.hidden = false;
        // The caption invites a press for live; with a picture behind it
        // that reads as a label for what is already there.
        var cap = container.querySelector('.caption');
        if (cap) cap.textContent = 'press ▶ for live';
      }
      next();
    }

    function draw() {
      if (!video.videoWidth || !video.videoHeight) return finish(false);
      try {
        var canvas = document.createElement('canvas');
        canvas.width = video.videoWidth;
        canvas.height = video.videoHeight;
        canvas.getContext('2d').drawImage(video, 0, 0);
        img.src = canvas.toDataURL('image/jpeg', 0.7);
        finish(true);
      } catch (e) {
        finish(false);
      }
    }

    // A camera that is configured but not delivering must not stall the
    // queue behind it.
    var timer = setTimeout(function () { finish(false); }, 8000);

    video.addEventListener('loadeddata', draw);
    try {
      player.attachMediaElement(video);
      player.load();
      // Muted and hidden: this is never shown playing, it exists to put
      // one frame on a canvas.
      var p = video.play();
      if (p && p.catch) p.catch(function () {});
    } catch (e) {
      finish(false);
    }
  }

  next();
};
