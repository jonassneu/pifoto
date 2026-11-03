package app.pifoto

import android.Manifest
import android.animation.LayoutTransition
import android.annotation.SuppressLint
import android.app.Activity
import android.app.NotificationManager
import android.content.Intent
import android.content.pm.PackageManager
import android.content.res.ColorStateList
import android.content.res.Configuration
import android.graphics.Insets
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.provider.Settings
import android.text.Editable
import android.text.InputType
import android.text.TextUtils
import android.text.TextWatcher
import android.view.Gravity
import android.view.View
import android.view.WindowInsets
import android.view.WindowInsetsController
import android.view.inputmethod.EditorInfo
import android.webkit.CookieManager
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.ImageButton
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView
import app.pifoto.BackupStatus.State

/**
 * First-run setup (log in → turn on backup → permission → all or only new → "You're all set")
 * and the Backup settings screen. Plain views styled after the PiFoto design boards.
 */
class SettingsActivity : Activity() {
    private lateinit var prefs: Prefs
    private lateinit var root: FrameLayout
    private lateinit var stage: FrameLayout
    private lateinit var sheets: Sheets
    private var screen: View? = null
    private var applyScreenInsets: (Insets, Int) -> Unit = { _, _ -> }
    private var sysInsets: Insets = Insets.NONE
    private var imeBottom = 0
    private var device = "phone"

    private var firstRun = false
    /** In the first-run flow after logging in: the sheets end on the "You're all set" screen. */
    private var onboarding = false
    /** Sent the user to the system's app settings to allow photo access; continue when they're back. */
    private var awaitingAppSettings = false

    private val handler = Handler(Looper.getMainLooper())
    private val ticker = object : Runnable {
        override fun run() {
            updateStatus()
            handler.postDelayed(this, if (shownState == State.RUN) 500 else 1000)
        }
    }
    private val serverTicker = object : Runnable {
        override fun run() { checkServer(); handler.postDelayed(this, 30_000) }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        prefs = Prefs(this)
        firstRun = intent.getBooleanExtra(EXTRA_FIRST_RUN, false)
        device = BackupStatus.device(this)
        if (Build.VERSION.SDK_INT < 35) @Suppress("DEPRECATION") window.setDecorFitsSystemWindows(false) // 15+: always edge-to-edge
        root = FrameLayout(this).apply { setBackgroundColor(Pal.BG) }
        stage = FrameLayout(this)
        root.addView(stage, FrameLayout.LayoutParams(-1, -1))
        sheets = Sheets(this, root)
        setContentView(root)
        window.insetsController?.setSystemBarsAppearance(0, // light icons on the dark bars
            WindowInsetsController.APPEARANCE_LIGHT_STATUS_BARS or WindowInsetsController.APPEARANCE_LIGHT_NAVIGATION_BARS)
        // Edge-to-edge: every screen pads itself clear of status bar, nav bar, cutout and keyboard.
        root.setOnApplyWindowInsetsListener { _, wi ->
            sysInsets = wi.getInsets(WindowInsets.Type.systemBars() or WindowInsets.Type.displayCutout())
            imeBottom = wi.getInsets(WindowInsets.Type.ime()).bottom
            applyInsets()
            WindowInsets.CONSUMED
        }

        if (prefs.loggedIn) showSettings(animate = false) else showLogin(animate = false)
    }

    override fun onResume() {
        super.onResume()
        Updater.onShow(this)
        handler.post(ticker)
        if (awaitingAppSettings) {
            awaitingAppSettings = false
            when {
                Backup.hasMediaPermission(this) -> chooseStartPoint()
                onboarding -> showDone(Choice.OFF, 0)
                else -> refresh()
            }
        } else refresh()
        if (screenKind == Kind.SETTINGS) handler.post(serverTicker)
    }

    override fun onPause() {
        handler.removeCallbacks(ticker)
        handler.removeCallbacks(serverTicker)
        Updater.onHide(this)
        super.onPause()
    }

    override fun onConfigurationChanged(newConfig: Configuration) {
        super.onConfigurationChanged(newConfig)
        device = BackupStatus.device(this)
        loginTop?.let { it.setPadding(it.paddingLeft, loginTopPad(), it.paddingRight, it.paddingBottom) }
    }

    @SuppressLint("GestureBackNavigation") // Android 12 and older; 13+ uses the sheets' OnBackInvokedCallback
    @Deprecated("Used on Android 12 and older")
    override fun onBackPressed() {
        if (!sheets.onBack()) @Suppress("DEPRECATION") super.onBackPressed()
    }

    // ---------- screens ----------

    private enum class Kind { LOGIN, SETTINGS, DONE }
    private var screenKind: Kind? = null

    /** Cross-fades to [v] (the design's .stage: opacity + a 0.97 scale on the spring). */
    private fun setScreen(kind: Kind, v: View, animate: Boolean, insets: (Insets, Int) -> Unit) {
        val old = screen
        screen = v; screenKind = kind; applyScreenInsets = insets
        stage.addView(v, FrameLayout.LayoutParams(-1, -1))
        applyInsets()
        if (old == null) return
        if (!animate) { stage.removeView(old); return }
        old.animate().alpha(0f).scaleX(.97f).scaleY(.97f).setDuration(260).setInterpolator(Motion.EO)
            .withEndAction { stage.removeView(old) }.start()
        v.alpha = 0f; v.scaleX = .97f; v.scaleY = .97f
        v.animate().alpha(1f).scaleX(1f).scaleY(1f).setDuration(520).setInterpolator(Motion.SPRING).start()
    }

    private fun applyInsets() {
        applyScreenInsets(sysInsets, imeBottom)
        sheets.setInsets(sysInsets.left, sysInsets.right, maxOf(sysInsets.bottom, imeBottom))
    }

    /** Scroll pages: bars as padding; the keyboard shrinks the page so the focused field scrolls into view. */
    private fun scrollInsets(scroll: ScrollView, top: Boolean) = { b: Insets, ime: Int ->
        scroll.setPadding(b.left, if (top) b.top else 0, b.right, if (ime > 0) 0 else b.bottom)
        (scroll.layoutParams as? LinearLayout.LayoutParams)?.let { it.bottomMargin = ime; scroll.layoutParams = it }
            ?: (scroll.layoutParams as? FrameLayout.LayoutParams)?.let { it.bottomMargin = ime; scroll.layoutParams = it }
        Unit
    }

    private fun scroller() = ScrollView(this).apply {
        isFillViewport = true; clipToPadding = false; isVerticalScrollBarEnabled = false
        overScrollMode = View.OVER_SCROLL_IF_CONTENT_SCROLLS
    }

    private fun host(url: String) = url.substringAfter("://").trimEnd('/')

    // ---------- login ----------

    private lateinit var serverField: EditText
    private lateinit var nameField: EditText
    private lateinit var pwField: EditText
    private lateinit var serverBox: LinearLayout
    private lateinit var nameBox: LinearLayout
    private lateinit var pwBox: LinearLayout
    private lateinit var errorRow: LinearLayout
    private lateinit var errorText: TextView
    private lateinit var loginBtn: Pill
    private lateinit var loginStatus: TextView
    private var loginTop: FrameLayout? = null
    private var loginBusy = false
    /** Which field the error line is about (red border); null = no error. */
    private var errorOn: Field? = null
    private enum class Field { SERVER, NAME, PASSWORD, LOGIN /* name or password: both */ }

    private fun loginTopPad() = dp(if (resources.configuration.screenHeightDp >= 600) 112 else 32)
    private val stayLine get() = "You'll stay logged in on this device for a year."

    private fun showLogin(animate: Boolean) {
        // The server ended the session (password reset, logged out elsewhere…): say so instead of "Welcome".
        val ended = prefs.problem == BackupStatus.PROBLEM_AUTH
        val col = Column(this, dp(340))
        col.add(ImageView(this).apply {
            setImageResource(R.drawable.ic_mark); importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO
        }, width = dp(56), height = dp(56))
        col.add(label(when { ended -> "Log in again"; firstRun -> "Welcome to PiFoto"; else -> "Log in to PiFoto" },
            30f, 700, spacing = -0.02f, line = 1.15f).apply { isAccessibilityHeading = true }, 16f)
        col.add(label(when {
            ended -> "You were logged out on this $device. Log in again to keep backing up your photos."
            firstRun -> "PiFoto keeps your photos on your family's Raspberry Pi. Log in with your name and password, then turn on automatic backup."
            else -> "Log in with your name and password to back up photos from this $device."
        }, 15f, color = Pal.MUT), 8f)

        val form = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            layoutTransition = LayoutTransition() // the error line fades in and the button moves down smoothly
        }
        serverField = input().apply {
            setText(prefs.server); hint = Prefs.DEFAULT_SERVER
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_URI
            imeOptions = EditorInfo.IME_ACTION_NEXT
            contentDescription = "Server address"
        }
        serverBox = fieldBox(serverField)
        form.add(fieldGroup("Server address · leave as it is", serverBox))

        nameField = input().apply {
            setText(prefs.name); hint = "Your name"
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_FLAG_CAP_WORDS or InputType.TYPE_TEXT_VARIATION_PERSON_NAME
            imeOptions = EditorInfo.IME_ACTION_NEXT
            setAutofillHints(View.AUTOFILL_HINT_USERNAME)
            contentDescription = "Name"
            setOnEditorActionListener { _, a, _ -> if (a == EditorInfo.IME_ACTION_NEXT) { pwField.requestFocus(); true } else false }
        }
        nameBox = fieldBox(nameField)
        form.add(fieldGroup("Name", nameBox), 16f)

        pwField = input().apply {
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
            imeOptions = EditorInfo.IME_ACTION_DONE
            setAutofillHints(View.AUTOFILL_HINT_PASSWORD)
            contentDescription = "Password"
            setOnEditorActionListener { _, a, _ -> if (a == EditorInfo.IME_ACTION_DONE) { login(); true } else false }
        }
        val eye = ImageButton(this).apply {
            setImageResource(R.drawable.ic_eye)
            imageTintList = ColorStateList.valueOf(Pal.MUT)
            scaleType = ImageView.ScaleType.CENTER_INSIDE
            setPadding(dp(12), dp(12), dp(12), dp(12))
            background = ripple(null, rounded(-1, dpf(12f)))
            contentDescription = "Show password"
            isClickable = true; isFocusable = true
            setOnClickListener {
                val show = (pwField.inputType and InputType.TYPE_MASK_VARIATION) != InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD
                val sel = pwField.selectionEnd
                pwField.inputType = InputType.TYPE_CLASS_TEXT or
                    if (show) InputType.TYPE_TEXT_VARIATION_VISIBLE_PASSWORD else InputType.TYPE_TEXT_VARIATION_PASSWORD
                pwField.typeface = font(400)
                pwField.setSelection(sel.coerceIn(0, pwField.length()))
                setImageResource(if (show) R.drawable.ic_eye_off else R.drawable.ic_eye)
                contentDescription = if (show) "Hide password" else "Show password"
            }
        }
        pwBox = fieldBox(pwField, eye)
        form.add(fieldGroup("Password", pwBox), 16f)

        errorText = label("", 13f, 500, Pal.BAD_TEXT, line = 1.3f).apply { accessibilityLiveRegion = View.ACCESSIBILITY_LIVE_REGION_ASSERTIVE }
        errorRow = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL; gravity = Gravity.CENTER_VERTICAL
            addView(icon(R.drawable.ic_alert, 16f, Pal.BAD_TEXT).apply { (layoutParams as LinearLayout.LayoutParams).marginEnd = dp(6) })
            addView(errorText, LinearLayout.LayoutParams(0, -2, 1f))
            visibility = View.GONE
        }
        form.add(errorRow, 10f)

        loginBtn = Pill(this, "Log in", Pill.Style.PRIMARY).apply { setOnClickListener { login() } }
        form.add(loginBtn, 24f)
        loginStatus = label(stayLine, 13f, color = Pal.MUT).apply {
            gravity = Gravity.CENTER; minHeight = dp(18); accessibilityLiveRegion = View.ACCESSIBILITY_LIVE_REGION_POLITE
        }
        form.add(loginStatus, 16f)
        // New family members (and anyone whose password was reset) choose a password via their invite link first.
        form.add(label(if (ended) "Was your password reset? Open the new invite link you were sent first."
            else "No password yet? Open the invite link you were sent first.", 13f, color = Pal.MUT, line = 1.35f)
            .apply { gravity = Gravity.CENTER }, 8f)
        col.add(form, 32f)

        val watcher = object : TextWatcher {
            override fun beforeTextChanged(s: CharSequence?, start: Int, count: Int, after: Int) {}
            override fun onTextChanged(s: CharSequence?, start: Int, before: Int, count: Int) {}
            override fun afterTextChanged(s: Editable?) { if (errorOn != null) setLoginError(null, null) }
        }
        serverField.addTextChangedListener(watcher)
        nameField.addTextChangedListener(watcher)
        pwField.addTextChangedListener(watcher)

        val pad = FrameLayout(this).apply { setPadding(dp(28), loginTopPad(), dp(28), dp(32)) }
        pad.addView(col, FrameLayout.LayoutParams(-1, -2, Gravity.CENTER_HORIZONTAL))
        loginTop = pad
        pad.isFocusableInTouchMode = true // so no field starts out focused (and ringed) before it's touched
        val scroll = scroller().apply { addView(pad) }
        loginBusy = false; errorOn = null
        setScreen(Kind.LOGIN, scroll, animate, scrollInsets(scroll, top = true))
        pad.requestFocus()
    }

    private fun input() = EditText(this).apply {
        background = null
        setPadding(0, 0, 0, 0)
        textSize = 16f
        typeface = font(400)
        setTextColor(ColorStateList(arrayOf(intArrayOf(-android.R.attr.state_enabled), IntArray(0)), intArrayOf(Pal.MUT, Pal.FG)))
        setHintTextColor(Pal.HINT)
        isSingleLine = true
    }

    private fun fieldBox(edit: EditText, trailing: View? = null) = LinearLayout(this).apply {
        orientation = LinearLayout.HORIZONTAL
        gravity = Gravity.CENTER_VERTICAL
        setPadding(dp(16), 0, if (trailing != null) dp(4) else dp(16), 0)
        addView(edit, LinearLayout.LayoutParams(0, -1, 1f))
        if (trailing != null) addView(trailing, LinearLayout.LayoutParams(dp(44), dp(44)))
        edit.setOnFocusChangeListener { _, _ -> styleFields() }
        background = fieldBg(focused = false, error = false)
    }

    private fun fieldGroup(title: String, box: View) = LinearLayout(this).apply {
        orientation = LinearLayout.VERTICAL
        add(label(title, 13f, 500, Pal.MUT, line = 1.3f).apply { importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO })
        add(box, 6f, height = dp(52))
    }

    private fun fieldBg(focused: Boolean, error: Boolean) = rounded(Pal.FIELD, dpf(14f),
        if (focused) dp(1.5f) else dp(1), when { error -> Pal.ERR_BORDER; focused -> Pal.ACC; else -> Pal.FIELD_LINE })

    private fun styleFields() {
        val e = errorOn
        serverBox.background = fieldBg(serverField.hasFocus(), e == Field.SERVER)
        nameBox.background = fieldBg(nameField.hasFocus(), e == Field.NAME || e == Field.LOGIN)
        pwBox.background = fieldBg(pwField.hasFocus(), e == Field.PASSWORD || e == Field.LOGIN)
    }

    /** Shows [msg] under the password and marks [on] (msg null clears it). */
    private fun setLoginError(msg: String?, on: Field?) {
        errorOn = if (msg != null) on else null
        styleFields()
        if (msg == null) { errorRow.visibility = View.GONE; return }
        errorText.text = msg
        errorRow.visibility = View.VISIBLE
        when (on) {
            Field.SERVER -> serverBox.shake()
            Field.NAME -> nameBox.shake()
            Field.LOGIN -> { nameBox.shake(); pwBox.shake() }
            else -> pwBox.shake()
        }
    }

    private fun setLoginBusy(b: Boolean, server: String = "") {
        loginBusy = b
        loginBtn.setBusy(b)
        serverField.isEnabled = !b
        nameField.isEnabled = !b
        pwField.isEnabled = !b
        loginStatus.text = if (b) "Connecting to ${host(server)}…" else stayLine
    }

    private fun login() {
        if (loginBusy) return
        val server = Prefs.normalizeServer(serverField.text.toString())
        // An empty name logs in the server's owner (the only account before family accounts), but the
        // server only allows that at home: over the public https address it needs the name.
        val name = nameField.text.toString().trim()
        val pw = pwField.text.toString()
        if (pw.isEmpty()) { setLoginError("Enter your password", Field.PASSWORD); return }
        // The password never goes to the plain-HTTP home address: on another network anything may answer
        // to that name, and nothing proves it's the Pi before logging in. With the address left as it
        // is, log in over the public https address instead (the same Pi, reachable from anywhere, home
        // included); afterwards the app switches to the home address whenever it proves to be the Pi
        // (Api.pickBase). Any other address (a test server, say) is the user's choice: log in exactly there.
        val base = if (server == Prefs.DEFAULT_SERVER) prefs.remote else server
        if (name.isEmpty() && base.startsWith("https://")) { setLoginError("Enter your name", Field.NAME); return }
        setLoginError(null, null)
        setLoginBusy(true, base)
        Thread {
            val result = try { Result.success(Api.login(base, name, pw, prefs.device)) } catch (e: Exception) { Result.failure(e) }
            runOnUiThread {
                if (isFinishing || isDestroyed) return@runOnUiThread
                setLoginBusy(false)
                result.onSuccess { r ->
                    val token = r.session
                    prefs.server = server
                    prefs.session = token
                    prefs.name = r.name.ifEmpty { name } // as the server spells it ("mama" → "Mama")
                    if (r.device.isNotEmpty()) prefs.device = r.device
                    prefs.problem = "" // e.g. "log in again" after the server ended the old session
                    // Only an https base tells the away address (see Api.remoteAddress); "" keeps the old one.
                    Thread { try { Api.remoteAddress(base, token).let { if (it.isNotEmpty()) prefs.remote = it } } catch (_: Exception) {}
                             runOnUiThread { refresh() } }.start()
                    nameField.setText(prefs.name)
                    pwField.setText("")
                    getSystemService(NotificationManager::class.java).cancel(Backup.NOTIF_AUTH_ID)
                    Backup.schedule(this)
                    if (firstRun && !prefs.backupEnabled) { onboarding = true; askEnableBackup() }
                    else showSettings(animate = true)
                }.onFailure { e ->
                    val net = e is java.net.UnknownHostException || e is java.net.ConnectException ||
                        e is java.net.SocketTimeoutException || e is java.net.NoRouteToHostException
                    // Plain http to anything but the home address is blocked (network_security_config.xml).
                    val cleartext = e is java.net.UnknownServiceException
                    val tls = e is javax.net.ssl.SSLException // e.g. a Wi-Fi sign-in page in the way
                    val wrong = e is Api.WrongLoginException
                    setLoginError(when {
                        cleartext -> "Plain http:// only works for ${host(Prefs.DEFAULT_SERVER)}. Use the https:// address."
                        tls -> "No secure connection to ${host(base)}. If this Wi-Fi asks you to sign in, do that first."
                        net && base == server -> "Can't reach ${host(base)}. Check the address and that this $device is online."
                        net -> "Can't reach PiFoto. Is this $device connected to the internet?"
                        else -> e.message ?: e.javaClass.simpleName
                    }, when { net || cleartext || tls -> Field.SERVER; wrong -> Field.LOGIN; else -> Field.PASSWORD })
                }
            }
        }.start()
    }

    private fun logout() {
        val who = prefs.name.let { if (it.isNotEmpty()) "You're logged in as $it. " else "" }
        sheets.show("logout", "Log out?", sheets.body("Log out?", "${who}Automatic backup stops until you log in again.", listOf(
            Pill(this, "Log out", Pill.Style.DANGER).apply { setOnClickListener { doLogout() } },
            Pill(this, "Cancel", Pill.Style.TONAL).apply { setOnClickListener { sheets.hide() } },
        )))
    }

    private fun doLogout() {
        sheets.hide()
        val session = prefs.session
        // Like every request with the session: to the home address only if it proves to be the Pi,
        // else the https one (which also works away from home, where the home address never did).
        Thread { Api.logout(Api.pickBase(prefs, session), session) }.start()
        prefs.session = ""
        prefs.problem = ""
        CookieManager.getInstance().removeAllCookies(null)
        Backup.schedule(this)
        showLogin(animate = true)
    }

    /** The server no longer knows our session (password reset, person removed, logged out elsewhere). */
    private fun sessionEnded(session: String) {
        if (!prefs.sessionEnded(session)) return
        CookieManager.getInstance().removeAllCookies(null)
        Backup.schedule(this) // stops the backup work until someone logs in again
        handler.removeCallbacks(serverTicker)
        sheets.hide()
        showLogin(animate = true)
    }

    // ---------- backup settings ----------

    private lateinit var statusHead: LinearLayout
    private lateinit var statusBadge: FrameLayout
    private lateinit var statusIcon: ImageView
    private lateinit var statusTitle: TextView
    private lateinit var statusSub: TextView
    private lateinit var progress: ProgressLine
    private lateinit var checkText: TextView
    private lateinit var totalText: TextView
    private lateinit var nowBtn: Pill
    private lateinit var backupRow: SwitchRow
    private lateinit var wifiRow: SwitchRow
    private lateinit var chargeRow: SwitchRow
    private lateinit var keepNote: TextView
    private lateinit var accountTitle: TextView
    private lateinit var accountSub: TextView
    private lateinit var homeHost: TextView
    private lateinit var homeState: TextView
    private lateinit var awayHost: TextView
    private lateinit var awayNote: TextView
    private lateinit var awayState: TextView
    private var shownState: State? = null
    private var shownKey = ""
    private var nowAction: () -> Unit = {}

    private enum class Reach { UNKNOWN, HOME, AWAY, NONE }
    private var reach = Reach.UNKNOWN
    private var checking = false

    private fun card() = LinearLayout(this).apply {
        orientation = LinearLayout.VERTICAL
        background = rounded(Pal.SURFACE, dpf(22f))
        clipToOutline = true
    }

    private fun sectionTitle(t: String) = label(t, 13f, 600, Pal.MUT, line = 1.3f).apply {
        isAccessibilityHeading = true; setPadding(dp(4), 0, dp(4), 0)
    }

    private fun showSettings(animate: Boolean) {
        onboarding = false
        val maxW = dp(600)
        val page = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }

        // Top bar: back to the library + title.
        val bar = Column(this, maxW).apply {
            orientation = LinearLayout.HORIZONTAL; gravity = Gravity.CENTER_VERTICAL
            setPadding(dp(4), 0, dp(16), 0)
        }
        bar.addView(ImageButton(this).apply {
            setImageResource(R.drawable.ic_back); imageTintList = ColorStateList.valueOf(Pal.FG)
            scaleType = ImageView.ScaleType.CENTER_INSIDE; setPadding(dp(12), dp(12), dp(12), dp(12))
            background = ripple(null, rounded(-1, 999f))
            contentDescription = "Back to library"
            isClickable = true; isFocusable = true
            setOnClickListener { openMain() }
        }, LinearLayout.LayoutParams(dp(48), dp(48)).apply { marginEnd = dp(4) })
        bar.addView(label("Backup settings", 20f, 650, spacing = -0.01f, line = 1.2f).apply { isAccessibilityHeading = true })
        val barWrap = FrameLayout(this)
        barWrap.addView(bar, FrameLayout.LayoutParams(-1, dp(64), Gravity.CENTER_HORIZONTAL))
        page.addView(barWrap)

        val col = Column(this, maxW).apply { setPadding(dp(16), dp(8), dp(16), dp(32)) }

        // Status card.
        val status = card().apply { setPadding(dp(20), dp(20), dp(20), dp(20)) }
        statusIcon = icon(R.drawable.ic_cloud_check, 24f, Pal.ACC)
        statusBadge = FrameLayout(this).apply {
            background = rounded(Pal.ACC_TINT, dpf(24f))
            addView(statusIcon, FrameLayout.LayoutParams(dp(24), dp(24), Gravity.CENTER))
        }
        statusTitle = label("", 19f, 650, spacing = -0.01f, line = 1.25f)
        statusSub = label("", 14f, color = Pal.MUT, line = 1.35f).apply { fontFeatureSettings = "tnum" }
        statusHead = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL; gravity = Gravity.CENTER_VERTICAL
            addView(statusBadge, LinearLayout.LayoutParams(dp(48), dp(48)).apply { marginEnd = dp(14) })
            addView(LinearLayout(context).apply {
                orientation = LinearLayout.VERTICAL
                addView(statusTitle); add(statusSub, 2f)
            }, LinearLayout.LayoutParams(0, -2, 1f))
        }
        status.add(statusHead)
        progress = ProgressLine(this)
        status.add(progress, 16f, height = dp(6))
        checkText = label("", 13f, color = Pal.MUT, line = 1.3f).apply { fontFeatureSettings = "tnum" }
        totalText = label("", 13f, color = Pal.MUT, line = 1.3f).apply {
            fontFeatureSettings = "tnum"; gravity = Gravity.END; maxLines = 1; ellipsize = TextUtils.TruncateAt.END
        }
        status.add(spread(checkText, totalText), 16f)
        nowBtn = Pill(this, "Back up now", Pill.Style.TONAL, R.drawable.ic_refresh, heightDp = 48f, textSize = 15f)
            .apply { setOnClickListener { nowAction() } }
        status.add(nowBtn, 16f)
        col.add(status)

        // Automatic backup.
        col.add(sectionTitle("Automatic backup"), 28f)
        val rules = card()
        backupRow = SwitchRow(this, "Back up new photos & videos", "To your photos, in original quality") { on ->
            if (on) enableBackup() else setBackup(false)
        }
        wifiRow = SwitchRow(this, "Only on Wi-Fi") { prefs.wifiOnly = it; Backup.schedule(this); updateStatus() }
        chargeRow = SwitchRow(this, "Only while charging") { prefs.chargingOnly = it; Backup.schedule(this); updateStatus() }
        rules.addView(backupRow); rules.addView(divider()); rules.addView(wifiRow); rules.addView(divider()); rules.addView(chargeRow)
        col.add(rules, 10f)
        keepNote = label("The Pi keeps everything, even if you delete it from this $device.", 13f, color = Pal.MUT, line = 1.35f)
            .apply { setPadding(dp(4), 0, dp(4), 0) }
        col.add(keepNote, 14f)

        // Server: who is logged in (backups go to their library), then how the Pi is reached.
        col.add(sectionTitle("Server"), 28f)
        val server = card()
        accountTitle = label("", 16f, line = 1.3f).apply { maxLines = 1; ellipsize = TextUtils.TruncateAt.END }
        accountSub = label("", 13f, color = Pal.MUT, line = 1.3f)
        server.addView(LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL; gravity = Gravity.CENTER_VERTICAL
            setPadding(dp(20), dp(16), dp(20), dp(16))
            addView(icon(R.drawable.ic_person, 22f, Pal.MUT).apply { (layoutParams as LinearLayout.LayoutParams).marginEnd = dp(16) })
            addView(LinearLayout(context).apply {
                orientation = LinearLayout.VERTICAL
                addView(accountTitle); add(accountSub, 2f)
            }, LinearLayout.LayoutParams(0, -2, 1f))
        })
        server.addView(divider())
        homeHost = sub(""); homeState = stateLabel()
        server.addView(serverRow(R.drawable.ic_home, "At home", homeHost, null, homeState))
        server.addView(divider())
        awayHost = sub(""); awayNote = sub("Works on any internet connection"); awayState = stateLabel()
        server.addView(serverRow(R.drawable.ic_globe, "Away from home", awayHost, awayNote, awayState))
        col.add(server, 10f)

        // Actions.
        col.add(Pill(this, "Open PiFoto", Pill.Style.PRIMARY).apply { setOnClickListener { openMain() } }, 32f)
        col.add(Pill(this, "Log out", Pill.Style.GHOST).apply { setColor(Pal.BAD_TEXT); setOnClickListener { logout() } }, 8f)

        val scroll = scroller()
        val wrap = FrameLayout(this)
        wrap.addView(col, FrameLayout.LayoutParams(-1, -2, Gravity.CENTER_HORIZONTAL))
        scroll.addView(wrap)
        page.addView(scroll, LinearLayout.LayoutParams(-1, 0, 1f))

        shownState = null; shownKey = ""; reach = Reach.UNKNOWN
        setScreen(Kind.SETTINGS, page, animate) { b, ime ->
            barWrap.setPadding(b.left, b.top, b.right, 0)
            scrollInsets(scroll, top = false)(b, ime)
        }
        refresh()
        if (animate) { handler.removeCallbacks(serverTicker); handler.post(serverTicker) }
    }

    private fun sub(t: String) = label(t, 13f, color = Pal.MUT, line = 1.3f).apply {
        maxLines = 1; ellipsize = TextUtils.TruncateAt.END
    }

    private fun stateLabel() = label("", 13f, 600, Pal.ACC, line = 1.3f).apply {
        compoundDrawablePadding = dp(6); gravity = Gravity.CENTER_VERTICAL
    }

    private fun serverRow(iconRes: Int, title: String, host: TextView, note: TextView?, state: TextView) = LinearLayout(this).apply {
        orientation = LinearLayout.HORIZONTAL; gravity = Gravity.CENTER_VERTICAL
        setPadding(dp(20), dp(16), dp(20), dp(16))
        addView(icon(iconRes, 22f, Pal.MUT).apply { (layoutParams as LinearLayout.LayoutParams).marginEnd = dp(16) })
        addView(LinearLayout(context).apply {
            orientation = LinearLayout.VERTICAL
            addView(label(title, 16f, line = 1.3f))
            add(host, 2f)
            if (note != null) add(note, 2f)
        }, LinearLayout.LayoutParams(0, -2, 1f))
        addView(state, LinearLayout.LayoutParams(-2, -2).apply { marginStart = dp(12) })
    }

    private fun setReachLabel(v: TextView, connected: Boolean?, text: String = "") {
        if (connected == true) {
            v.text = "Connected"; v.setTextColor(Pal.ACC)
            v.setCompoundDrawablesRelative(getDrawable(R.drawable.ic_check)!!.mutate().apply {
                setTint(Pal.ACC); setBounds(0, 0, dp(16), dp(16))
            }, null, null, null)
        } else {
            v.text = text; v.setTextColor(Pal.MUT)
            v.setCompoundDrawablesRelative(null, null, null, null)
        }
        v.visibility = if (v.text.isEmpty()) View.GONE else View.VISIBLE
    }

    private fun renderServer() {
        val name = prefs.name
        accountTitle.text = if (name.isNotEmpty()) "Logged in as $name" else "Logged in"
        accountSub.text = "Backups from this $device go to your photos."
        homeHost.text = host(prefs.server)
        // Always https (Prefs.remote): the public address (Tailscale Funnel), no Tailscale app needed.
        awayHost.text = host(prefs.remote)
        when (reach) {
            Reach.UNKNOWN -> { setReachLabel(homeState, null, "Checking…"); setReachLabel(awayState, null) }
            Reach.HOME -> { setReachLabel(homeState, true); setReachLabel(awayState, null) }
            Reach.AWAY -> { setReachLabel(homeState, null, "No answer"); setReachLabel(awayState, true) }
            Reach.NONE -> { setReachLabel(homeState, null, "No answer"); setReachLabel(awayState, null, "No answer") }
        }
    }

    /**
     * Which address the app uses right now — the same rule as Api.pickBase: home only if it proves to
     * be our Pi, else the public one — and who is logged in there (GET /api/me), which also tells us
     * if the session has ended. (The away check sends no session; /api/me then goes to that https address.)
     */
    private fun checkServer() {
        if (checking || screenKind != Kind.SETTINGS || !prefs.loggedIn) return
        checking = true
        val home = prefs.server; val remote = prefs.remote; val session = prefs.session
        Thread {
            val homeOk = Api.verifyHome(home, session)
            val awayOk = !homeOk && Api.reachable(remote)
            var ended = false
            if (homeOk || awayOk) {
                val base = if (homeOk) home else remote
                try {
                    Api.me(base, session)?.name?.takeIf { it.isNotEmpty() && it != prefs.name }?.let { prefs.name = it }
                } catch (_: Api.AuthException) { ended = true } catch (_: Exception) {}
                if (!ended) try { // also learn a new away address (only told over https)
                    Api.remoteAddress(base, session).let { if (it.isNotEmpty() && it != prefs.remote) prefs.remote = it }
                } catch (_: Exception) {}
            }
            runOnUiThread {
                checking = false
                if (isDestroyed) return@runOnUiThread
                if (ended) { sessionEnded(session); return@runOnUiThread }
                if (screenKind != Kind.SETTINGS) return@runOnUiThread
                reach = when { homeOk -> Reach.HOME; awayOk -> Reach.AWAY; else -> Reach.NONE }
                renderServer()
            }
        }.start()
    }

    private fun refresh() {
        if (screenKind != Kind.SETTINGS || !prefs.loggedIn) return
        val on = prefs.backupEnabled && Backup.hasMediaPermission(this)
        backupRow.setChecked(on)
        wifiRow.setChecked(prefs.wifiOnly)
        chargeRow.setChecked(prefs.chargingOnly)
        wifiRow.isEnabled = on
        chargeRow.isEnabled = on
        keepNote.text = "The Pi keeps everything, even if you delete it from this $device."
        renderServer()
        updateStatus()
    }

    private fun updateStatus() {
        if (screenKind != Kind.SETTINGS || !prefs.loggedIn) return
        val s = BackupStatus.snapshot(this, device)
        val (title, sub) = when (s.state) {
            State.OFF -> "Backup is off" to "Turn it on below to back up new photos."
            State.RUN -> "Backing up · ${s.left} left" to s.file.ifEmpty { "Preparing…" }
            State.PAUSED -> when (s.problem) {
                BackupStatus.PROBLEM_NET -> "Couldn't reach PiFoto" to "Will retry soon. Is this ${s.device} connected to the internet?"
                BackupStatus.PROBLEM_FULL -> "Storage full" to "New photos wait until an admin gives you more space on the Pi."
                else -> "Backup paused" to s.reason
            }
            State.OK -> "Up to date" to (s.lastSummary.takeIf { it.isNotEmpty() && it != "Up to date" }
                ?: ("New photos upload automatically" + (if (s.wifiOnly) " on Wi-Fi" else "") +
                    (if (s.chargingOnly) " while charging" else "")))
        }
        val key = "${s.state}/${s.problem}"
        if (key != shownKey) {
            val (res, tint, color) = when (s.state) {
                State.RUN -> Triple(R.drawable.ic_cloud_up, Pal.ACC_TINT, Pal.ACC)
                State.OK -> Triple(R.drawable.ic_cloud_check, Pal.ACC_TINT, Pal.ACC)
                State.OFF -> Triple(R.drawable.ic_cloud_x, Pal.OFF_TINT, Pal.MUT)
                State.PAUSED -> Triple(R.drawable.ic_warn, Pal.WARN_TINT, Pal.WARN)
            }
            statusIcon.setImageResource(res)
            statusIcon.imageTintList = ColorStateList.valueOf(color)
            statusBadge.background = rounded(tint, dpf(24f))
            if (shownKey.isNotEmpty()) statusHead.rise(distDp = 6f, durationMs = 360)
            shownKey = key
            progress.visibility = if (s.state == State.RUN) View.VISIBLE else View.GONE
            if (s.state == State.RUN) progress.set(s.progress.toFloat().coerceAtLeast(0f), false)
            val perm = s.state == State.PAUSED && s.problem == BackupStatus.PROBLEM_PERM
            nowBtn.text.text = if (perm) "Allow photo access" else "Back up now"
            nowAction = if (perm) { { enableBackup() } } else { { Backup.runNow(this); updateStatus() } }
            nowBtn.isEnabled = s.state == State.OK || s.state == State.PAUSED
        }
        shownState = s.state
        statusTitle.text = title
        statusSub.text = sub
        if (s.state == State.RUN) progress.set(s.progress.toFloat().coerceAtLeast(0f), true)
        checkText.text = if (s.state == State.RUN && s.live != null) "Started ${BackupStatus.ago(s.live.startedAt)}"
            else BackupStatus.lastCheck(s.lastRun)
        totalText.text = "${BackupStatus.count(s.uploadedTotal)} uploaded from this ${s.device}"
    }

    // ---------- turning backup on: sheets ----------

    private var turnOnBtn: Pill? = null

    private fun askEnableBackup() {
        val turnOn = Pill(this, "Turn on", Pill.Style.PRIMARY).apply { setOnClickListener { setBusy(true); enableBackup() } }
        turnOnBtn = turnOn
        sheets.show("askBackup", "Turn on automatic backup?", sheets.body("Turn on automatic backup?",
            "New photos and videos from this $device will be backed up to your photos on the Pi automatically.", listOf(
                turnOn, Pill(this, "Not now", Pill.Style.GHOST).apply { setOnClickListener { backupDeclined() } }),
            iconRes = R.drawable.ic_cloud_up), onCancel = { backupDeclined() })
    }

    /** "Not now", or backed out of the permission / all-or-new steps. */
    private fun backupDeclined() {
        sheets.hide()
        if (onboarding) showDone(Choice.OFF, 0) else refresh()
    }

    private fun enableBackup() {
        val perms = mutableListOf<String>()
        if (Build.VERSION.SDK_INT >= 33) {
            perms += Manifest.permission.READ_MEDIA_IMAGES
            perms += Manifest.permission.READ_MEDIA_VIDEO
            perms += Manifest.permission.POST_NOTIFICATIONS
        } else perms += Manifest.permission.READ_EXTERNAL_STORAGE
        perms += Manifest.permission.ACCESS_MEDIA_LOCATION
        val missing = perms.filter { checkSelfPermission(it) != PackageManager.PERMISSION_GRANTED }
        if (missing.isEmpty()) chooseStartPoint() else requestPermissions(missing.toTypedArray(), REQ_PERMS)
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode != REQ_PERMS) return
        if (Backup.hasMediaPermission(this)) { chooseStartPoint(); return }
        val partial = Build.VERSION.SDK_INT >= 34 &&
            checkSelfPermission(Manifest.permission.READ_MEDIA_VISUAL_USER_SELECTED) == PackageManager.PERMISSION_GRANTED
        askForAccess(partial)
        refresh()
    }

    /** Photo access was denied (or only selected photos allowed): explain, offer the app's settings. */
    private fun askForAccess(partial: Boolean) {
        sheets.show("permission", "Photo access needed", sheets.body("Photo access needed",
            if (partial) "You allowed access to selected photos only. Automatic backup needs access to all photos and videos."
            else "Automatic backup needs access to your photos and videos.", listOf(
                Pill(this, "Open app settings", Pill.Style.PRIMARY).apply { setOnClickListener {
                    awaitingAppSettings = true
                    sheets.hide()
                    startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.fromParts("package", packageName, null)))
                } },
                Pill(this, "Not now", Pill.Style.GHOST).apply { setOnClickListener { backupDeclined() } }),
            iconRes = R.drawable.ic_photo, iconColor = Pal.WARN, iconTint = Pal.WARN_TINT,
            small = "In app settings: Permissions → Photos and videos → Always allow all."),
            onCancel = { backupDeclined() })
    }

    private fun chooseStartPoint() {
        turnOnBtn?.setBusy(true)
        Thread {
            val n = Backup.volumes(this).sumOf { v -> Backup.pendingOn(this, v, 0).count { it.name.isNotEmpty() } }
            runOnUiThread {
                if (isFinishing || isDestroyed) return@runOnUiThread
                turnOnBtn = null
                val ns = BackupStatus.count(n.toLong())
                sheets.show("askExisting", "Back up existing photos?", sheets.body("Back up existing photos?",
                    "There are $ns photos and videos on this device. Back them all up now, or only new ones from now on?", listOf(
                        Pill(this, "Back up all $ns", Pill.Style.PRIMARY).apply { setOnClickListener { setBackup(true, all = true, n = n) } },
                        Pill(this, "Only new ones", Pill.Style.TONAL).apply { setOnClickListener { setBackup(true, all = false) } }),
                    small = "Photos already in your library on the Pi are recognised and skipped."),
                    onCancel = { backupDeclined() })
            }
        }.start()
    }

    private fun setBackup(on: Boolean, all: Boolean = false, n: Int = 0) {
        prefs.backupEnabled = on
        if (on) {
            if (all) Backup.backupAllExisting(this) else Backup.skipExisting(this)
            prefs.lastStatus = ""
            prefs.problem = ""
        }
        Backup.schedule(this)
        if (on && all) Backup.runNow(this)
        sheets.hide()
        if (onboarding && on) showDone(if (all) Choice.ALL else Choice.NEW, n) else refresh()
    }

    // ---------- "You're all set" ----------

    private enum class Choice { ALL, NEW, OFF }

    private fun showDone(choice: Choice, n: Int) {
        onboarding = false
        val col = Column(this, dp(320)).apply { gravity = Gravity.CENTER_HORIZONTAL }
        val tick = DrawnCheck(this, Pal.ACC)
        val circle = FrameLayout(this).apply {
            background = rounded(Pal.ACC_TINT, dpf(44f))
            addView(tick, FrameLayout.LayoutParams(dp(44), dp(44), Gravity.CENTER))
        }
        col.add(circle, width = dp(88), height = dp(88))
        val title = label("You're all set", 26f, 700, spacing = -0.02f, line = 1.2f).apply {
            gravity = Gravity.CENTER; isAccessibilityHeading = true
        }
        col.add(title, 20f)
        val text = label(when (choice) {
            Choice.ALL -> "PiFoto is backing up ${BackupStatus.count(n.toLong())} photos and videos in the background. You can keep using your $device."
            Choice.NEW -> "New photos and videos will be backed up as you take them."
            Choice.OFF -> "Automatic backup is off. You can turn it on any time in Backup settings."
        }, 15f, color = Pal.MUT).apply { gravity = Gravity.CENTER }
        col.add(text, 20f)
        val open = Pill(this, "Open PiFoto", Pill.Style.PRIMARY).apply { setOnClickListener { openMain() } }
        col.add(open, 32f, width = -2, gravity = Gravity.CENTER_HORIZONTAL)

        val box = FrameLayout(this)
        box.addView(col, FrameLayout.LayoutParams(-1, -2, Gravity.CENTER))
        setScreen(Kind.DONE, box, animate = true) { b, _ ->
            box.setPadding(b.left + dp(28), b.top + dp(32), b.right + dp(28), b.bottom + dp(32))
        }
        circle.alpha = 0f; circle.scaleX = .6f; circle.scaleY = .6f
        circle.animate().alpha(1f).scaleX(1f).scaleY(1f).setDuration(560).setInterpolator(Motion.SPRING).start()
        tick.play(220)
        title.rise(0); text.rise(90); open.rise(180)
    }

    /** Back to the library: the existing one if it's underneath, else a fresh one. */
    private fun openMain() {
        if (!prefs.loggedIn) return
        startActivity(Intent(this, MainActivity::class.java)
            .addFlags(Intent.FLAG_ACTIVITY_CLEAR_TOP or Intent.FLAG_ACTIVITY_SINGLE_TOP))
        finish()
    }

    companion object {
        const val EXTRA_FIRST_RUN = "firstRun"
        private const val REQ_PERMS = 1
    }
}
