#!/usr/bin/env node
/*
 * compute-server.js - Mira Pi compute server (epic 10 follow-up: productionization)
 *
 * Serves the epic 10 T2 route contract on :8080 (MIRA_PI_PORT) for the
 * album-art pipeline:
 *
 *   GET /api/v1/capabilities -> {"tier":"compute"|"cache","disk_cache":bool,
 *                                "remote_colors":bool,"remote_blur":bool,
 *                                "host":...,"model":...}
 *   GET /img/<urlencoded-cdn-url>/160.jpg -> artwork
 *   GET /img/<urlencoded-cdn-url>/colors  -> {"dominant":[r,g,b]}
 *
 * CORS: Access-Control-Allow-Origin * on every response (the UI origin
 * http://localhost:80 is cross-origin to the Pi), OPTIONS preflight -> 204.
 *
 * Production behaviour (this follow-up, replacing the placeholder):
 *   160.jpg: fetch the CDN upstream (10 s idle timeout, 2 MB cap, up to 3
 *            redirects), JPEG-decode, cover-crop + box-downscale to 160x160,
 *            re-encode as JPEG q85.
 *   colors:  same upstream source (shared in-flight dedup: ONE fetch per
 *            URL, concurrent requests wait on the same promise), downscale
 *            to <=32x32, 4-bit quant histogram (16 levels/channel, 4096
 *            cells) -> dominant colour = mean of the most frequent cell.
 *   Disk cache: <cache>/<sha1(url)>/{160.jpg,colors.json} - content address
 *            is the URL (same directory convention as the old nginx alias).
 *            Hits are served straight from disk (no upstream). Eviction:
 *            when the directory count exceeds MIRA_CACHE_MAX_FILES the
 *            oldest directories (mtime) are removed; hits refresh their
 *            directory mtime (LRU-ish).
 *
 * Degradation is by design:
 *   - non-JPEG upstream (e.g. PNG) or a decode failure -> 160.jpg passes
 *     the original bytes through unchanged (200, pre-production behaviour)
 *     and colors answers the grey [128,128,128]. Degraded results are NOT
 *     written to the disk cache, so a later request may still fill the
 *     entry with a real result.
 *   - without jpeg-js (the npm install failed at setup time) the whole
 *     service runs in that degraded mode. Capabilities keep
 *     remote_colors/remote_blur = true on purpose: the UI already falls
 *     back to the CDN image + local colour extraction, so no client-side
 *     logic depends on a capability flip.
 *
 * jpeg-js is a pure-JS codec: no native build step. That is the reason it
 * is the image backend here instead of Sharp (native bindings need a
 * toolchain + build at setup time on armv6/armv7 - Raspbian/DietPi/Alpine/
 * Void, several of which ship node without one).
 *
 * Environment:
 *   MIRA_PI_PORT         listen port (default 8080)
 *   MIRA_PI_TIER         "compute" | "lightweight" (default "compute");
 *                        lightweight -> capabilities tier "cache" and a
 *                        smaller default cache cap (200 instead of 500)
 *   MIRA_PI_MODEL        model string reported in capabilities
 *   MIRA_CACHE_DIR       cache root (default /var/cache/mira/img; override
 *                        for tests)
 *   MIRA_CACHE_MAX_FILES eviction cap (defaults: 500 compute / 200
 *                        lightweight)
 *
 * Logging: plain console.log lines (the service runs under nohup into
 * /var/log/mira-compute.log, arranged by setup-pi.sh).
 *
 * Compatibility: Node >= 14, CommonJS, zero mandatory dependencies
 * (jpeg-js is optional; its absence is a degraded mode, not a crash).
 */
"use strict";

const http = require("http");
const https = require("https");
const fs = require("fs");
const path = require("path");
const os = require("os");
const crypto = require("crypto");

// optional image codec: without it the service keeps running in degraded
// mode (passthrough 160.jpg + grey colors), see header
let jpeg = null;
try {
  jpeg = require("jpeg-js");
} catch (e) {
  jpeg = null;
}

const PORT = parseInt(process.env.MIRA_PI_PORT || "8080", 10) || 8080;
const TIER = process.env.MIRA_PI_TIER || "compute";
const MODEL = process.env.MIRA_PI_MODEL || "";
const CACHE_DIR = process.env.MIRA_CACHE_DIR || "/var/cache/mira/img";
const UPSTREAM_TIMEOUT_MS = 10 * 1000;
const UPSTREAM_MAX_BYTES = 2 * 1024 * 1024;
const MAX_REDIRECTS = 3;
const JPEG_QUALITY = 85;
const OUT_SIZE = 160;
const COLOR_MAX = 32;

function cacheMaxFiles() {
  const v = parseInt(process.env.MIRA_CACHE_MAX_FILES, 10);
  if (v > 0) return v;
  return TIER === "lightweight" ? 200 : 500;
}
const CACHE_MAX_FILES = cacheMaxFiles();

// epic 10 T2 contract: {"tier":..., "disk_cache":..., "remote_colors":...,
// "remote_blur":...} (+ host/model as before)
const CAPS = {
  tier: TIER === "lightweight" ? "cache" : "compute",
  disk_cache: true,
  remote_colors: true,
  remote_blur: true,
  host: os.hostname(),
  model: MODEL
};

// /img/<urlencoded-cdn-url>/<160.jpg|colors> (epic 10 T2 contract); the
// encoded URL contains no raw slashes, so the path splits cleanly
const IMG_RE = /^\/img\/([^/]+)\/([^/]+)$/;

// ---------------------------------------------------------------- upstream

function doFetch(url, redirectsLeft) {
  return new Promise(function (resolve, reject) {
    let settled = false;
    const fail = function (err) {
      if (!settled) {
        settled = true;
        reject(err);
      }
    };
    const ok = function (v) {
      if (!settled) {
        settled = true;
        resolve(v);
      }
    };
    const mod = url.indexOf("https://") === 0 ? https : http;
    let req;
    try {
      req = mod.get(url, function (res) {
        const code = res.statusCode || 0;
        if (code >= 300 && code < 400 && res.headers.location) {
          res.resume();
          if (redirectsLeft <= 0) {
            fail(new Error("too many redirects from upstream"));
            return;
          }
          let next;
          try {
            next = new URL(res.headers.location, url).toString();
          } catch (e) {
            fail(new Error("bad redirect location from upstream"));
            return;
          }
          if (next.indexOf("http://") !== 0 && next.indexOf("https://") !== 0) {
            fail(new Error("redirect to non-http(s) url"));
            return;
          }
          doFetch(next, redirectsLeft - 1).then(ok, fail);
          return;
        }
        if (code !== 200) {
          res.resume();
          fail(new Error("upstream status " + code));
          return;
        }
        const chunks = [];
        let total = 0;
        res.on("data", function (c) {
          total += c.length;
          chunks.push(c);
          if (total > UPSTREAM_MAX_BYTES) {
            fail(new Error("upstream response exceeds " + UPSTREAM_MAX_BYTES + " bytes"));
            try {
              req.destroy();
            } catch (e) {
              /* ignore */
            }
          }
        });
        res.on("end", function () {
          if (settled) return;
          ok({
            statusCode: code,
            contentType: res.headers["content-type"] || "application/octet-stream",
            body: Buffer.concat(chunks)
          });
        });
        res.on("error", function (e) {
          fail(e);
        });
      });
    } catch (e) {
      fail(e);
      return;
    }
    req.setTimeout(UPSTREAM_TIMEOUT_MS, function () {
      fail(new Error("upstream timeout after " + UPSTREAM_TIMEOUT_MS + " ms"));
      try {
        req.destroy();
      } catch (e) {
        /* ignore */
      }
    });
    req.on("error", function (e) {
      fail(e);
    });
  });
}

// shared in-flight dedup: one fetch per URL, concurrent requests wait on
// the same promise (the CDN is not asked twice for the same artwork)
const inflight = new Map();

function fetchUpstream(url) {
  const pending = inflight.get(url);
  if (pending) return pending;
  const tracked = doFetch(url, MAX_REDIRECTS).then(
    function (v) {
      inflight.delete(url);
      return v;
    },
    function (e) {
      inflight.delete(url);
      throw e;
    }
  );
  inflight.set(url, tracked);
  return tracked;
}

// ---------------------------------------------------------------- disk cache

function cacheEntryDir(url) {
  const hash = crypto.createHash("sha1").update(url, "utf8").digest("hex");
  return path.join(CACHE_DIR, hash);
}

function readCacheFile(file) {
  try {
    return fs.readFileSync(file);
  } catch (e) {
    return null;
  }
}

// write via temp+rename so a concurrent reader never sees a partial file
function writeCacheFile(file, data) {
  try {
    fs.mkdirSync(path.dirname(file), { recursive: true });
    const tmp = file + ".tmp-" + process.pid;
    fs.writeFileSync(tmp, data);
    fs.renameSync(tmp, file);
  } catch (e) {
    console.log("cache write failed for " + file + ": " + e.message);
  }
}

function touchDir(dir) {
  try {
    const now = new Date();
    fs.utimesSync(dir, now, now);
  } catch (e) {
    /* mtime refresh is best-effort */
  }
}

function rmDir(p) {
  let entries;
  try {
    entries = fs.readdirSync(p);
  } catch (e) {
    return;
  }
  for (let i = 0; i < entries.length; i++) {
    const child = path.join(p, entries[i]);
    try {
      if (fs.statSync(child).isDirectory()) rmDir(child);
      else fs.unlinkSync(child);
    } catch (e) {
      /* skip vanished/locked child */
    }
  }
  try {
    fs.rmdirSync(p);
  } catch (e) {
    /* skip */
  }
}

// eviction: remove the oldest directories (mtime) until the count is at or
// below the cap (name as deterministic tie-break)
function evict() {
  let names;
  try {
    names = fs.readdirSync(CACHE_DIR);
  } catch (e) {
    return; // cache root unavailable - degraded, nothing to evict
  }
  const dirs = [];
  for (let i = 0; i < names.length; i++) {
    const p = path.join(CACHE_DIR, names[i]);
    try {
      const st = fs.statSync(p);
      if (st.isDirectory()) dirs.push({ p: p, mtime: st.mtimeMs, name: names[i] });
    } catch (e) {
      /* vanished meanwhile */
    }
  }
  if (dirs.length <= CACHE_MAX_FILES) return;
  dirs.sort(function (a, b) {
    if (a.mtime !== b.mtime) return a.mtime - b.mtime;
    return a.name < b.name ? -1 : 1;
  });
  const n = dirs.length - CACHE_MAX_FILES;
  for (let i = 0; i < n; i++) rmDir(dirs[i].p);
  console.log("cache eviction: removed " + n + " oldest entr(y/ies), " + (dirs.length - n) + " kept (cap " + CACHE_MAX_FILES + ")");
}

// ---------------------------------------------------------------- image work

// JPEG-decode; null = not a JPEG (e.g. PNG) or decode failure
function decodeImage(body) {
  if (!jpeg) return null;
  try {
    return jpeg.decode(body, { useTArray: false, maxMemoryUsageInMB: 256 });
  } catch (e) {
    return null;
  }
}

// box filter (area average) resize of RGBA buffers. Real box averaging on
// downscale; for upscale (source smaller than the target) the boxes shrink
// to < 1 px and the filter degenerates to nearest neighbour.
function boxResize(data, sw, sh, tw, th) {
  const out = new Uint8Array(tw * th * 4);
  for (let ty = 0; ty < th; ty++) {
    const sy0 = Math.floor((ty * sh) / th);
    const sy1 = Math.max(sy0 + 1, Math.ceil(((ty + 1) * sh) / th));
    for (let tx = 0; tx < tw; tx++) {
      const sx0 = Math.floor((tx * sw) / tw);
      const sx1 = Math.max(sx0 + 1, Math.ceil(((tx + 1) * sw) / tw));
      let r = 0;
      let g = 0;
      let b = 0;
      let n = 0;
      for (let y = sy0; y < sy1; y++) {
        let i = (y * sw + sx0) * 4;
        for (let x = sx0; x < sx1; x++, i += 4) {
          r += data[i];
          g += data[i + 1];
          b += data[i + 2];
          n++;
        }
      }
      const o = (ty * tw + tx) * 4;
      out[o] = Math.round(r / n);
      out[o + 1] = Math.round(g / n);
      out[o + 2] = Math.round(b / n);
      out[o + 3] = 255;
    }
  }
  return out;
}

// cover-crop the source to a centred 1:1 box, box-resize it to OUT_SIZE,
// re-encode as JPEG; returns the encoded bytes
function render160(decoded) {
  const sw = decoded.width;
  const sh = decoded.height;
  const scale = Math.max(OUT_SIZE / sw, OUT_SIZE / sh);
  const cropW = Math.min(sw, Math.max(1, Math.round(OUT_SIZE / scale)));
  const cropH = Math.min(sh, Math.max(1, Math.round(OUT_SIZE / scale)));
  const x0 = Math.max(0, Math.round((sw - cropW) / 2));
  const y0 = Math.max(0, Math.round((sh - cropH) / 2));
  const crop = new Uint8Array(cropW * cropH * 4);
  for (let y = 0; y < cropH; y++) {
    const s = ((y0 + y) * sw + x0) * 4;
    const d = y * cropW * 4;
    crop.set(decoded.data.subarray(s, s + cropW * 4), d);
  }
  const resized = boxResize(crop, cropW, cropH, OUT_SIZE, OUT_SIZE);
  const enc = jpeg.encode({ data: resized, width: OUT_SIZE, height: OUT_SIZE }, JPEG_QUALITY);
  return Buffer.from(enc.data);
}

// dominant colour: downscale to <=32x32 (aspect-preserving), 4-bit quant
// histogram, mean of the most frequent cell; [128,128,128] as the last
// resort (also the legacy placeholder answer)
function extractDominant(decoded) {
  const sw = decoded.width;
  const sh = decoded.height;
  let tw;
  let th;
  if (sw >= sh) {
    tw = COLOR_MAX;
    th = Math.max(1, Math.round((sh * COLOR_MAX) / sw));
  } else {
    th = COLOR_MAX;
    tw = Math.max(1, Math.round((sw * COLOR_MAX) / sh));
  }
  const sample = boxResize(decoded.data, sw, sh, tw, th);
  const counts = new Int32Array(4096);
  const rsum = new Int32Array(4096);
  const gsum = new Int32Array(4096);
  const bsum = new Int32Array(4096);
  for (let p = 0; p < tw * th; p++) {
    const r = sample[p * 4];
    const g = sample[p * 4 + 1];
    const b = sample[p * 4 + 2];
    const cell = ((r >> 4) << 8) | ((g >> 4) << 4) | (b >> 4);
    counts[cell]++;
    rsum[cell] += r;
    gsum[cell] += g;
    bsum[cell] += b;
  }
  let best = -1;
  for (let c = 0; c < 4096; c++) {
    if (counts[c] > 0 && (best < 0 || counts[c] > counts[best])) best = c;
  }
  if (best < 0) return [128, 128, 128];
  const n = counts[best];
  return [Math.round(rsum[best] / n), Math.round(gsum[best] / n), Math.round(bsum[best] / n)];
}

// ---------------------------------------------------------------- http

function sendJson(res, status, obj) {
  const body = Buffer.from(JSON.stringify(obj));
  res.writeHead(status, {
    "Content-Type": "application/json",
    "Content-Length": body.length
  });
  res.end(body);
}

function sendBytes(res, status, contentType, body) {
  res.writeHead(status, {
    "Content-Type": contentType,
    "Content-Length": body.length
  });
  res.end(body);
}

function handle160(url, res) {
  const dir = cacheEntryDir(url);
  const cached = readCacheFile(path.join(dir, "160.jpg"));
  if (cached) {
    touchDir(dir); // LRU-ish: a hit is a recent use
    sendBytes(res, 200, "image/jpeg", cached);
    return;
  }
  fetchUpstream(url)
    .then(function (up) {
      const decoded = decodeImage(up.body);
      if (!decoded) {
        // non-JPEG source (e.g. PNG) or decode failure: pass the original
        // bytes through (pre-production behaviour, 200). NOT cached, so a
        // later request can still fill the entry with a real result.
        sendBytes(res, 200, up.contentType, up.body);
        return;
      }
      let out;
      try {
        out = render160(decoded);
      } catch (e) {
        sendBytes(res, 200, up.contentType, up.body);
        return;
      }
      writeCacheFile(path.join(dir, "160.jpg"), out);
      touchDir(dir);
      evict();
      sendBytes(res, 200, "image/jpeg", out);
    })
    .catch(function (e) {
      console.log("160.jpg: upstream fetch failed for " + url + ": " + e.message);
      sendJson(res, 502, { error: "upstream fetch failed" });
    });
}

function handleColors(url, res) {
  const dir = cacheEntryDir(url);
  const cached = readCacheFile(path.join(dir, "colors.json"));
  if (cached) {
    touchDir(dir);
    sendBytes(res, 200, "application/json", cached);
    return;
  }
  fetchUpstream(url)
    .then(function (up) {
      const decoded = decodeImage(up.body);
      if (!decoded) {
        // legacy grey answer (pre-production behaviour); not cached
        sendJson(res, 200, { dominant: [128, 128, 128] });
        return;
      }
      let dom;
      try {
        dom = extractDominant(decoded);
      } catch (e) {
        sendJson(res, 200, { dominant: [128, 128, 128] });
        return;
      }
      const body = Buffer.from(JSON.stringify({ dominant: dom }));
      writeCacheFile(path.join(dir, "colors.json"), body);
      touchDir(dir);
      evict();
      sendBytes(res, 200, "application/json", body);
    })
    .catch(function (e) {
      console.log("colors: upstream fetch failed for " + url + ": " + e.message);
      sendJson(res, 502, { error: "upstream fetch failed" });
    });
}

function main() {
  // cache root is best-effort: the service must keep running without it
  try {
    fs.mkdirSync(CACHE_DIR, { recursive: true });
  } catch (e) {
    console.log("WARNING: cache dir " + CACHE_DIR + " not available (" + e.message + ") - running without disk cache");
  }
  if (!jpeg) {
    console.log("WARNING: jpeg-js is not installed - degraded mode: 160.jpg passthrough + grey dominant colors (UI fallbacks apply)");
  } else {
    console.log("jpeg-js loaded: real 160-resize + color extraction active");
  }
  console.log(
    "mira compute-server starting: tier=" + TIER +
    " port=" + PORT +
    " cacheDir=" + CACHE_DIR +
    " cacheMaxFiles=" + CACHE_MAX_FILES +
    " model=" + (MODEL || "(unset)")
  );

  const server = http.createServer(function (req, res) {
    // the UI (http://localhost:80) is cross-origin to the Pi (epic 10 T2)
    res.setHeader("Access-Control-Allow-Origin", "*");
    if (req.method === "OPTIONS") {
      res.writeHead(204, {
        "Access-Control-Allow-Methods": "GET, OPTIONS",
        "Access-Control-Allow-Headers": "*"
      });
      res.end();
      return;
    }
    if (req.method !== "GET") {
      sendJson(res, 404, { error: "not found" });
      return;
    }
    let u;
    try {
      u = new URL(req.url, "http://localhost");
    } catch (e) {
      sendJson(res, 404, { error: "not found" });
      return;
    }
    if (u.pathname === "/api/v1/capabilities") {
      sendJson(res, 200, CAPS);
      return;
    }
    const m = u.pathname.match(IMG_RE);
    if (m) {
      let src;
      try {
        src = decodeURIComponent(m[1]);
      } catch (e) {
        src = m[1];
      }
      if (src.indexOf("http://") !== 0 && src.indexOf("https://") !== 0) {
        sendJson(res, 400, { error: "invalid cdn url in path" });
        return;
      }
      if (m[2] === "160.jpg" || m[2] === "160") {
        handle160(src, res);
        return;
      }
      if (m[2] === "colors") {
        handleColors(src, res);
        return;
      }
    }
    sendJson(res, 404, { error: "not found" });
  });

  server.on("error", function (e) {
    console.log("server error: " + e.message);
    process.exit(1);
  });
  server.listen(PORT, "0.0.0.0", function () {
    console.log("mira compute-server listening on :" + PORT + " (tier=" + CAPS.tier + ")");
  });
}

// keep the long-lived Pi service alive across unexpected handler errors
// (upstream failures are already handled per-request; this is the last
// line before a lost album-art endpoint)
process.on("uncaughtException", function (e) {
  console.log("uncaught exception (service kept alive): " + ((e && e.stack) || String(e)));
});

main();
