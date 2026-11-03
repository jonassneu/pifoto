package app.pifoto

import android.annotation.SuppressLint
import android.app.Activity
import android.app.DownloadManager
import android.content.ClipData
import android.content.ClipboardManager
import android.content.ActivityNotFoundException
import android.content.Intent
import android.content.res.Configuration
import android.net.ConnectivityManager
import android.net.Network
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Environment
import android.os.ext.SdkExtensions
import android.provider.MediaStore
import android.view.View
import android.view.WindowInsets
import android.view.WindowInsetsController
import android.webkit.CookieManager
import android.webkit.JavascriptInterface
import android.webkit.URLUtil
import android.webkit.ValueCallback
import android.webkit.WebChromeClient
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.FrameLayout
import android.widget.Toast
import android.window.OnBackInvokedDispatcher
import org.json.JSONObject

/** The PiFoto web app in a WebView, plus native file picking, downloads and a settings bridge. */
class MainActivity : Activity() {
    private lateinit var web: WebView
    private lateinit var prefs: Prefs
    private var fileCallback: ValueCallback<Array<Uri>>? = null
    /** Address the page was loaded from: always the checked home address or the https one (Api.pickBase). */
    private var loadedServer = ""
    /** Origin of [loadedServer], for the bridge (read from the WebView's JS thread). */
    @Volatile private var loadedOrigin = ""
    /** Origin of the page in the main frame: set on the UI thread as pages start, read by the bridge. */
    @Volatile private var frameOrigin = ""
    /** Session the page was loaded with; a different one (logged in again in settings) needs a reload. */
    private var loadedSession = ""
    /** "phone" / "tablet", for the bridge (read from the WebView's JS thread). */
    @Volatile private var device = "phone"

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        if (!prefs.loggedIn) {
            startActivity(Intent(this, SettingsActivity::class.java).putExtra(SettingsActivity.EXTRA_FIRST_RUN, true))
            finish()
            return
        }

        device = BackupStatus.device(this)
        // Android 15+ is edge-to-edge anyway; older versions opt in.
        if (Build.VERSION.SDK_INT < 35) @Suppress("DEPRECATION") window.setDecorFitsSystemWindows(false)
        val root = FrameLayout(this).apply { setBackgroundColor(Pal.BG) }
        web = WebView(this).apply { setBackgroundColor(Pal.BG) }
        root.addView(web, FrameLayout.LayoutParams(-1, -1))
        setContentView(root)

        // Edge-to-edge is enforced on new Android versions: keep content clear of system bars and keyboard.
        root.setOnApplyWindowInsetsListener { v, insets ->
            val b = insets.getInsets(WindowInsets.Type.systemBars() or WindowInsets.Type.displayCutout() or WindowInsets.Type.ime())
            v.setPadding(b.left, b.top, b.right, b.bottom)
            WindowInsets.CONSUMED
        }
        window.insetsController?.setSystemBarsAppearance(0,
            WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS or WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS)

        web.settings.apply {
            javaScriptEnabled = true
            domStorageEnabled = true
            mediaPlaybackRequiresUserGesture = true
            allowFileAccess = false
            allowContentAccess = false
        }
        web.addJavascriptInterface(Bridge(), "PiFotoApp")
        web.webViewClient = Client()
        web.webChromeClient = Chrome()
        web.setDownloadListener { url, _, disposition, mime, _ -> download(url, disposition, mime) }

        if (Build.VERSION.SDK_INT >= 33) {
            onBackInvokedDispatcher.registerOnBackInvokedCallback(OnBackInvokedDispatcher.PRIORITY_DEFAULT) { goBack() }
        }
        load(force = true)
        watchNetwork()
    }

    private var picking = false
    private var forceQueued = false // a forced load was asked for while an address check was running
    private var resumed = false
    private var netCallback: ConnectivityManager.NetworkCallback? = null
    private var defaultNet: Network? = null
    /** Counts network changes: a home check that ran on an older network says nothing about this one. */
    @Volatile private var netGen = 0
    /** A network change took the plain-HTTP home page's cookie back; the next load checks and sets it again. */
    private var cookieWithdrawn = false
    /** The "Can't reach PiFoto" page is up: any re-check (resume, network change) reloads. */
    private var showingError = false
    /** Failed loads in a row, so the page can say "Still no answer". */
    private var failStreak = 0
    private var errorThisLoad = false

    /**
     * Picks the address to use right now — the home address if it proves to be our Pi (direct,
     * fast), otherwise the public https one — and (re)loads the page if it differs from what's
     * shown. [force] reloads regardless.
     */
    private fun load(force: Boolean = false) {
        if (picking) { if (force) forceQueued = true; return }
        adoptPageSession()
        picking = true
        val session = prefs.session
        val gen = netGen
        Thread {
            val base = Api.pickBase(prefs, session)
            // The answer is about the network and session it was checked with.
            val current = { gen == netGen && prefs.session == session }
            runOnUiThread {
                picking = false
                if (isFinishing || isDestroyed) return@runOnUiThread
                if (!current()) { load(force = force); return@runOnUiThread } // changed meanwhile: check again
                val f = force || forceQueued || showingError || cookieWithdrawn
                forceQueued = false
                if (!f && base == loadedServer) return@runOnUiThread
                loadedServer = base
                loadedOrigin = Api.origin(base)
                loadedSession = session
                setSessionCookie(base, session)
                cookieWithdrawn = false
                web.loadUrl("$base/")
            }
            if (!current()) return@Thread
            // Learn/refresh the away-from-home address from the server (only over https, see Api.remoteAddress).
            // After loading, so an unreachable Pi doesn't hold up the page (or the Retry spinner).
            try { Api.remoteAddress(base, session).let { if (it.isNotEmpty() && it != prefs.remote) prefs.remote = it } }
            catch (_: Exception) {}
            // Who is logged in (for settings and the next login), and is the session still valid?
            try { Api.me(base, session)?.name?.takeIf { it.isNotEmpty() && it != prefs.name }?.let { prefs.name = it } }
            catch (_: Api.AuthException) { runOnUiThread { sessionEnded(session) } }
            catch (_: Exception) {} // offline or an old server: nothing to learn
        }.start()
    }

    /**
     * The server no longer knows [session] (password reset, person removed, logged out elsewhere):
     * hand over to the native "Log in again" screen instead of the page's own login. In the
     * background only the session is dropped; onResume then opens the login.
     */
    private fun sessionEnded(session: String) {
        if (isFinishing || isDestroyed || !prefs.sessionEnded(session)) return
        Backup.schedule(this) // stops the backup work until someone logs in again
        if (resumed) {
            startActivity(Intent(this, SettingsActivity::class.java).putExtra(SettingsActivity.EXTRA_FIRST_RUN, true))
            finish()
        }
    }

    /**
     * The session cookie goes only to the address about to be loaded (checked home or https), with
     * the same attributes the server gives it. The other address's copy is deleted: e.g. the
     * plain-HTTP home one once away, where anyone may answer to that name.
     */
    private fun setSessionCookie(base: String, session: String) {
        val cm = CookieManager.getInstance()
        cm.setAcceptCookie(true)
        for (b in listOf(prefs.server, prefs.remote).distinct()) if (b != base) expireCookie(b)
        cm.setCookie(base, "${Api.COOKIE}=$session; Path=/; HttpOnly; SameSite=Strict" +
            if (base.startsWith("https://")) "; Secure" else "")
        cm.flush()
    }

    private fun expireCookie(base: String) {
        CookieManager.getInstance().setCookie(base, "${Api.COOKIE}=; Path=/; Max-Age=0" +
            if (base.startsWith("https://")) "; Secure" else "")
    }

    /** Leaving/arriving home switches networks: re-check which address to use. */
    private fun watchNetwork() {
        val cm = getSystemService(ConnectivityManager::class.java)
        defaultNet = cm.activeNetwork // so the callback's first report (this same network) is no change
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                runOnUiThread { if (network != defaultNet) { defaultNet = network; networkChanged() } }
            }
            override fun onLost(network: Network) {
                runOnUiThread { if (network == defaultNet) { defaultNet = null; networkChanged() } }
            }
        }
        cm.registerDefaultNetworkCallback(cb)
        netCallback = cb
    }

    /**
     * The plain-HTTP home page was checked on the old network; on the new one anything may answer
     * to its name, and the page keeps making requests. So its cookie is taken back at once (after
     * keeping a session the page got by itself); the next load checks again and sets it only if
     * this is still home.
     */
    private fun networkChanged() {
        if (isDestroyed) return
        netGen++
        if (loadedServer.startsWith("http://") && !cookieWithdrawn) {
            adoptPageSession()
            expireCookie(loadedServer)
            CookieManager.getInstance().flush()
            cookieWithdrawn = true
        }
        if (resumed) load()
    }

    override fun onResume() {
        super.onResume()
        resumed = true
        Updater.onShow(this)
        if (prefs.loggedIn) { Updater.schedule(this); Updater.checkSoon(this) }
        if (!::web.isInitialized) return
        web.onResume()
        if (!prefs.loggedIn) { // logged out from settings
            startActivity(Intent(this, SettingsActivity::class.java).putExtra(SettingsActivity.EXTRA_FIRST_RUN, true))
            finish()
        } else {
            // Reloads only if the reachable address changed (e.g. left home), the session changed
            // (logged in again in settings) or the "Can't reach" page is showing.
            load(force = prefs.session != loadedSession)
            Backup.runNow(this) // app is visible: a good moment to sync (may use a foreground service)
        }
    }

    override fun onConfigurationChanged(newConfig: Configuration) {
        super.onConfigurationChanged(newConfig)
        device = BackupStatus.device(this)
    }

    override fun onPause() {
        resumed = false
        if (::web.isInitialized) { web.onPause(); syncSession() }
        Updater.onHide(this)
        super.onPause()
    }

    override fun onDestroy() {
        netCallback?.let { getSystemService(ConnectivityManager::class.java).unregisterNetworkCallback(it) }
        if (::web.isInitialized) web.destroy()
        super.onDestroy()
    }

    /**
     * The session cookie the page has right now ("" if none). Only from [loadedServer], and not
     * while a network change has made it unchecked: there a fake server could plant a cookie.
     */
    private fun pageSession(): String {
        if (loadedServer.isEmpty() || cookieWithdrawn) return ""
        val cookies = CookieManager.getInstance().getCookie(loadedServer) ?: ""
        return cookies.split(';').map { it.trim() }
            .firstOrNull { it.startsWith("${Api.COOKIE}=") }?.substringAfter('=') ?: ""
    }

    /**
     * Someone logged in inside the web page (possibly as another person): use that session from now on.
     * Only a token the page got by itself counts: the one it was loaded with may be stale (logged in
     * again in settings, or the server ended it).
     */
    private fun adoptPageSession(token: String = pageSession()): Boolean {
        if (token.isEmpty() || token == prefs.session || token == loadedSession) return false
        prefs.session = token
        loadedSession = token // the page already uses it
        if (prefs.problem == BackupStatus.PROBLEM_AUTH) prefs.problem = ""
        Backup.schedule(this)
        return true
    }

    /** If the user logged in/out inside the web page, mirror that session for the backup worker. */
    private fun syncSession() {
        if (loadedServer.isEmpty() || cookieWithdrawn) return // withdrawn: gone because of us, not a logout
        val token = pageSession()
        if (adoptPageSession(token)) return
        if (token.isEmpty() && prefs.loggedIn) {
            // Cookie gone: the user probably logged out in the page. Confirm with the server (checked
            // again: the network may have changed since the page was loaded).
            val session = prefs.session
            Thread {
                val valid = try { Api.checkSession(Api.pickBase(prefs, session), session) } catch (_: Exception) { true } // offline: keep
                if (!valid && prefs.sessionEnded(session)) Backup.schedule(applicationContext)
            }.start()
        }
    }

    /**
     * Back within the page's own address only: an older entry may be the other address (home before
     * leaving, where something else may answer now) or the app's "Can't reach" page.
     */
    private fun goBack() {
        val list = web.copyBackForwardList()
        val prev = if (list.currentIndex > 0) list.getItemAtIndex(list.currentIndex - 1)?.url else null
        val home = Api.origin(loadedServer)
        if (prev != null && home.isNotEmpty() && Api.origin(prev) == home) web.goBack() else finish()
    }

    @SuppressLint("GestureBackNavigation") // fallback for Android 12 and older; 13+ uses OnBackInvokedCallback
    @Deprecated("Used on Android 12 and older")
    override fun onBackPressed() = goBack()

    // ---------- downloads ----------

    private fun download(url: String, disposition: String?, mime: String?) {
        val uri = Uri.parse(url)
        if (uri.scheme?.lowercase() !in setOf("http", "https")) { // DownloadManager can't fetch blob:/data: URLs
            Toast.makeText(this, "Can't download this file", Toast.LENGTH_SHORT).show()
            return
        }
        val name = URLUtil.guessFileName(url, disposition, mime)
        val own = Api.origin(loadedServer).let { it.isNotEmpty() && it == Api.origin(url) }
        // DownloadManager runs in another process and resumes on whatever network comes next, outside
        // the app's home check: a download from the plain-HTTP home address goes over the https address
        // instead, so the session only ever travels encrypted (and only to the server the page came from).
        val src = if (own && !url.startsWith("https://")) prefs.remote + (uri.encodedPath ?: "/") +
            (uri.encodedQuery?.let { "?$it" } ?: "") else url
        val req = DownloadManager.Request(Uri.parse(src))
        if (own && !cookieWithdrawn && src.startsWith("https://")) req.addRequestHeader("Cookie", "${Api.COOKIE}=${prefs.session}")
        req.setTitle(name)
            .setMimeType(mime)
            .setNotificationVisibility(DownloadManager.Request.VISIBILITY_VISIBLE_NOTIFY_COMPLETED)
            .setDestinationInExternalPublicDir(Environment.DIRECTORY_PICTURES, "PiFoto/$name")
        getSystemService(DownloadManager::class.java).enqueue(req)
        Toast.makeText(this, "Downloading $name to Pictures/PiFoto", Toast.LENGTH_SHORT).show()
    }

    // ---------- file chooser (web "Upload" button) ----------

    private inner class Chrome : WebChromeClient() {
        override fun onShowFileChooser(view: WebView, callback: ValueCallback<Array<Uri>>,
                                       params: FileChooserParams): Boolean {
            fileCallback?.onReceiveValue(null)
            fileCallback = callback
            val multi = params.mode == FileChooserParams.MODE_OPEN_MULTIPLE
            // System photo picker (Android 13+, or 11/12 with the R extension 2): no permission needed.
            val hasPicker = Build.VERSION.SDK_INT >= 33 || SdkExtensions.getExtensionVersion(Build.VERSION_CODES.R) >= 2
            return try {
                if (!hasPicker) throw ActivityNotFoundException()
                val picker = Intent(MediaStore.ACTION_PICK_IMAGES)
                if (multi) picker.putExtra(MediaStore.EXTRA_PICK_IMAGES_MAX, MediaStore.getPickImagesMaxLimit())
                startActivityForResult(picker, REQ_PICK); true
            } catch (_: ActivityNotFoundException) {
                try {
                    startActivityForResult(params.createIntent().putExtra(Intent.EXTRA_ALLOW_MULTIPLE, multi), REQ_PICK); true
                } catch (_: ActivityNotFoundException) {
                    fileCallback = null; false
                }
            }
        }
    }

    @Deprecated("Simple and sufficient here")
    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode != REQ_PICK) return
        val uris = mutableListOf<Uri>()
        if (resultCode == RESULT_OK && data != null) {
            val clip = data.clipData
            if (clip != null) for (i in 0 until clip.itemCount) uris += clip.getItemAt(i).uri
            else data.data?.let { uris += it }
        }
        fileCallback?.onReceiveValue(if (uris.isEmpty()) null else uris.toTypedArray())
        fileCallback = null
    }

    // ---------- navigation + errors ----------

    /**
     * Remembers whose page is in the main frame, for the bridge. about:blank and data: keep the
     * previous value: they are the app's own "Can't reach" page (see onReceivedError) or take the
     * origin of the page that opened them. Anything else that isn't http(s) is trusted with nothing.
     */
    private fun trackFrame(url: String?) {
        if (url == null || url == "about:blank" || url.startsWith("data:")) return
        frameOrigin = Api.origin(url)
    }

    /** The bridge only works for PiFoto itself: the main frame must show a page from [loadedServer]. */
    private fun trusted(): Boolean = frameOrigin.isNotEmpty() && frameOrigin == loadedOrigin

    private inner class Client : WebViewClient() {
        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
            val u = request.url
            val home = Api.origin(loadedServer)
            if (home.isNotEmpty() && Api.origin(u.toString()) == home) return false
            // Anything else leaves the app, but only a web link the user tapped: no intent:, market:,
            // sms:… and nothing a script starts by itself.
            val isWeb = u.scheme?.lowercase().let { it == "http" || it == "https" }
            if (isWeb && request.hasGesture()) {
                try { startActivity(Intent(Intent.ACTION_VIEW, u).addCategory(Intent.CATEGORY_BROWSABLE)) }
                catch (_: ActivityNotFoundException) {}
            }
            return true
        }

        override fun onPageStarted(view: WebView, url: String?, favicon: android.graphics.Bitmap?) {
            trackFrame(url)
            if (url?.startsWith("http") == true) errorThisLoad = false
        }

        override fun doUpdateVisitedHistory(view: WebView, url: String?, isReload: Boolean) { trackFrame(url) }

        override fun onPageFinished(view: WebView, url: String?) {
            if (url?.startsWith("http") == true && !errorThisLoad) { showingError = false; failStreak = 0 }
        }

        override fun onReceivedError(view: WebView, request: WebResourceRequest, error: WebResourceError) {
            if (!request.isForMainFrame) return
            errorThisLoad = true
            showingError = true
            failStreak++
            val code = (error.description?.toString() ?: "").removePrefix("net::")
            // The app's own page replaces the failed one; its Retry and Settings buttons need the bridge.
            frameOrigin = loadedOrigin
            view.loadDataWithBaseURL(null, cantReachPage(loadedServer.substringAfter("://"), code,
                tried = failStreak > 1), "text/html", "utf-8", null)
        }
    }

    /**
     * Native "Can't reach PiFoto" page (System board tokens). Retry shows a spinner while the app
     * re-checks home/away and reloads; it also retries by itself every 10 s.
     */
    private fun cantReachPage(host: String, code: String, tried: Boolean): String {
        val esc = { t: String -> android.text.Html.escapeHtml(t) }
        val sub = esc(host) + if (code.isNotEmpty()) " · " + esc(code) else ""
        // Away from home (or whenever the home address can't prove it's the Pi) the app uses the public
        // address, which needs internet on both ends: this device's and the one at home.
        val online = "Is this $device online? (Wi-Fi or mobile data)"
        val homeNet = "Away from home: is the internet at home working?"
        val rise = if (tried) "" else " rise" // only animate in the first time, not on every auto-retry
        val sp = "linear(0,.044,.142,.26,.38,.49,.588,.671,.74,.796,.841,.877,.905,.927,.944,.958,.968,.976,.982,.986,.99,.992,.994,.996,.997,.998,.998,.999,1)"
        return """<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
:root{color-scheme:dark;--sp:$sp;--eo:cubic-bezier(.2,0,0,1)}
*{box-sizing:border-box}
html,body{margin:0;min-height:100%;background:#111214;color:#ececee;font:15px/1.4 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;-webkit-font-smoothing:antialiased;-webkit-tap-highlight-color:transparent}
body{display:flex;justify-content:center;align-items:center;min-height:100vh;padding:24px 28px 40px}
main{width:100%;max-width:420px;display:flex;flex-direction:column;gap:28px}
.ic{fill:none;stroke:currentColor;stroke-width:1.8;stroke-linecap:round;stroke-linejoin:round}
.badge{width:64px;height:64px;border-radius:999px;background:rgba(255,179,92,.14);color:#ffb35c;display:flex;align-items:center;justify-content:center}
h1{margin:0;font-size:26px;font-weight:700;letter-spacing:-.02em;line-height:1.15}
.sub{margin:6px 0 0;font-size:13px;color:#a0a2a9;font-variant-numeric:tabular-nums;overflow-wrap:anywhere}
ul{list-style:none;margin:0;padding:4px 0;background:#1b1c20;border-radius:22px}
li{display:flex;gap:14px;align-items:center;padding:14px 18px}
li+li{border-top:1px solid rgba(255,255,255,.06)}
li svg{flex:none;color:#a0a2a9}
.btns{display:flex;flex-direction:column;gap:8px}
button{appearance:none;border:0;margin:0;font:inherit;color:inherit;height:52px;border-radius:999px;font-size:16px;cursor:pointer;position:relative;transition:transform 140ms var(--eo),background-color 160ms var(--eo)}
button:active{transform:scale(.97)}
button:focus-visible{outline:2px solid #6aa8ff;outline-offset:2px}
.pri{background:#6aa8ff;color:#071223;font-weight:650}
.sec{background:none;box-shadow:inset 0 0 0 1px #3a3b42;font-weight:600}
.sec:active{background:rgba(255,255,255,.1)}
.lbl,.spin{display:block;transition:opacity 160ms var(--eo),transform 300ms var(--sp)}
.busy .lbl{opacity:0;transform:scale(.9)}
.spin{position:absolute;left:50%;top:50%;width:22px;height:22px;margin:-11px 0 0 -11px;opacity:0;transform:scale(.9)}
.busy .spin{opacity:1;transform:none}
.spin svg{display:block;animation:spin .9s linear infinite}
@keyframes spin{to{transform:rotate(360deg)}}
.note{margin:8px 0 0;text-align:center;font-size:13px;color:#a0a2a9;font-variant-numeric:tabular-nums;min-height:18px}
.rise{animation:rise 620ms var(--sp) both}.d2{animation-delay:80ms}.d3{animation-delay:160ms}
@keyframes rise{from{opacity:0;transform:translateY(14px)}}
@keyframes fade{from{opacity:0}}
@media (prefers-reduced-motion:reduce){.rise{animation:fade 220ms both}.lbl,.spin{transform:none!important}}
</style></head><body><main>
<div style="display:flex;flex-direction:column;align-items:flex-start;gap:16px">
<div class="badge$rise"><svg class="ic" width="30" height="30" viewBox="0 0 24 24" aria-hidden="true"><path d="M3 3l18 18"/><path d="M8.6 5.4A6 6 0 0 1 17.6 9.54 4.5 4.5 0 0 1 20.4 17"/><path d="M17 18.5H7a4.5 4.5 0 0 1-.8-8.93"/></svg></div>
<div class="d2$rise"><h1>Can't reach PiFoto</h1><p class="sub">$sub</p></div>
</div>
<ul class="d3$rise">
<li><svg class="ic" width="22" height="22" viewBox="0 0 24 24" aria-hidden="true"><path d="M12 3v8"/><path d="M6.3 6.8a8 8 0 1 0 11.4 0"/></svg><span>Is the Raspberry Pi on?</span></li>
<li><svg class="ic" width="22" height="22" viewBox="0 0 24 24" aria-hidden="true"><circle cx="12" cy="12" r="8.5"/><path d="M3.5 12h17"/><path d="M12 3.5c2.4 2.4 3.5 5.3 3.5 8.5s-1.1 6.1-3.5 8.5c-2.4-2.4-3.5-5.3-3.5-8.5s1.1-6.1 3.5-8.5z"/></svg><span>${esc(online)}</span></li>
<li><svg class="ic" width="22" height="22" viewBox="0 0 24 24" aria-hidden="true"><path d="M4 11l8-6 8 6"/><path d="M6 10v9h12v-9"/></svg><span>${esc(homeNet)}</span></li>
</ul>
<div class="btns d3$rise">
<button id="retry" class="pri"><span class="lbl">Retry</span><span class="spin" aria-hidden="true"><svg width="22" height="22" viewBox="0 0 24 24"><circle cx="12" cy="12" r="9" fill="none" stroke="rgba(7,18,35,.2)" stroke-width="2.6"/><path d="M12 3a9 9 0 0 1 9 9" fill="none" stroke="#071223" stroke-width="2.6" stroke-linecap="round"/></svg></span></button>
<button id="settings" class="sec">Settings</button>
<p id="note" class="note" role="status"></p>
</div>
</main>
<script>
(function(){
  var host = ${JSONObject.quote(host)}, tried = $tried, secs = 10, busy = false, safety = 0;
  var btn = document.getElementById('retry'), note = document.getElementById('note');
  function render(){
    note.textContent = busy ? 'Trying ' + host + '\u2026'
      : (tried ? 'Still no answer \u00b7 trying again in ' : 'Trying again in ') + secs + ' s';
  }
  function retry(){
    if (busy) return;
    busy = true; btn.classList.add('busy'); btn.setAttribute('aria-busy', 'true'); render();
    // The app reloads this page with the result; if nothing happens, go back to counting down.
    clearTimeout(safety);
    safety = setTimeout(function(){ busy = false; tried = true; secs = 10; btn.classList.remove('busy'); btn.removeAttribute('aria-busy'); render(); }, 60000);
    try { PiFotoApp.retry(); } catch (e) {}
  }
  btn.onclick = retry;
  document.getElementById('settings').onclick = function(){ try { PiFotoApp.openSettings(); } catch (e) {} };
  setInterval(function(){
    if (busy || document.hidden) return;
    if (--secs <= 0) retry(); else render();
  }, 1000);
  render();
})();
</script></body></html>"""
    }

    /**
     * Exposed to the page as window.PiFotoApp. Every method first checks [trusted], so a page from
     * anywhere else (a link that slipped through, an old history entry) can't start backups, read
     * the backup status or fill the clipboard. Runs on the WebView's JS thread: both origins it
     * compares are volatile.
     */
    private inner class Bridge {
        /** Re-check home/away and reload (the "Can't reach" page's Retry). Ignored in the background: onResume reloads. */
        @JavascriptInterface
        fun retry() { if (trusted()) runOnUiThread { if (resumed) load(force = true) } }

        @JavascriptInterface
        fun openSettings() {
            if (trusted()) runOnUiThread { startActivity(Intent(this@MainActivity, SettingsActivity::class.java)) }
        }

        /** Starts a backup pass now (no-op if backup is off or logged out; one pass at a time). */
        @JavascriptInterface
        fun runBackup() { if (trusted()) Backup.runNow(applicationContext) }

        /**
         * The page logged in, out or joined by itself, or got a 401: the app takes over that session
         * for backups, or (if it has ended) shows its own "Log in again" screen.
         */
        @JavascriptInterface
        fun sessionChanged() { if (trusted()) runOnUiThread { if (resumed) load() } }

        /** Android share sheet for [text], e.g. an invite link (navigator.share needs https). */
        @JavascriptInterface
        fun share(text: String) {
            if (trusted()) runOnUiThread {
                val send = Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, text)
                try { startActivity(Intent.createChooser(send, null)) } catch (_: ActivityNotFoundException) {}
            }
        }

        /** Copies [text] to the clipboard (navigator.clipboard needs https). Android shows its own confirmation. */
        @JavascriptInterface
        fun copy(text: String) {
            if (trusted()) runOnUiThread {
                getSystemService(ClipboardManager::class.java).setPrimaryClip(ClipData.newPlainText("PiFoto", text))
            }
        }

        /**
         * JSON for the web header's backup chip and popover:
         * {"enabled","state":"off|ok|run|paused","title","detail","file","progress":0..1|-1,"left",
         *  "uploadedTotal","device":"tablet|phone","wifiOnly","chargingOnly","lastRun"}.
         * Called on the WebView's JS thread: reads only in-memory state, never the network.
         * "null" for a page that isn't trusted (the web app then shows no backup chip).
         */
        @JavascriptInterface
        fun backupStatus(): String = if (trusted()) BackupStatus.json(applicationContext, device) else "null"
    }

    companion object {
        private const val REQ_PICK = 1
    }
}
