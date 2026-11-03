package app.pifoto

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import android.content.pm.PackageManager
import android.os.Build
import android.util.Log
import androidx.work.Constraints
import androidx.work.CoroutineWorker
import androidx.work.ExistingPeriodicWorkPolicy
import androidx.work.ExistingWorkPolicy
import androidx.work.NetworkType
import androidx.work.OneTimeWorkRequestBuilder
import androidx.work.PeriodicWorkRequestBuilder
import androidx.work.WorkManager
import androidx.work.WorkerParameters
import java.io.File
import java.io.IOException
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext

/**
 * The app keeps itself up to date from the Pi (tools/publish-app.sh publishes a new version):
 * every 6 hours on Wi-Fi, and when the app is opened (at most hourly), it asks /api/app. A newer
 * version is downloaded and installed once nobody is using the app and no backup is uploading.
 * On Android 12+ the app may update itself without asking (UPDATE_PACKAGES_WITHOUT_USER_ACTION);
 * where Android wants a confirmation, a notification asks for one tap.
 */
object Updater {
    private const val PERIODIC = "pifoto-update"
    private const val SOON = "pifoto-update-soon"
    private const val CHANNEL = "updates"
    const val NOTIF_READY = 3
    const val NOTIF_DONE = 4
    const val ACTION_STATUS = "app.pifoto.UPDATE_STATUS"
    private const val TAG = "PiFotoUpdate"

    /** One check at a time (the periodic one and the app-opened one can start together). */
    val busy = AtomicBoolean(false)

    /** Android asks for a tap and someone is here to give it: show its screen (not more than daily if they say no). */
    fun askNow(p: Prefs) = p.updateNeedsTap && shown > 0 && System.currentTimeMillis() - p.updateDeclined > 86_400_000L

    /** Activities of this app on screen right now (an update replaces the running app). */
    @Volatile var shown = 0
        private set

    fun onShow(ctx: Context) {
        shown++
        if (askNow(Prefs(ctx))) checkSoon(ctx, delaySec = 2, force = true)
    }
    fun onHide(ctx: Context) {
        shown = (shown - 1).coerceAtLeast(0)
        // Left the app with an update waiting: install it shortly (coming back in time defers it again).
        if (shown == 0 && Prefs(ctx).updateWaiting) checkSoon(ctx, delaySec = 20, force = true)
    }

    fun installed(ctx: Context): Pair<Long, String> {
        val pi = ctx.packageManager.getPackageInfo(ctx.packageName, 0)
        return pi.longVersionCode to (pi.versionName ?: "")
    }

    /** (Re)arms the 6-hourly check, or stops it when logged out. */
    fun schedule(ctx: Context) {
        val wm = WorkManager.getInstance(ctx)
        if (!Prefs(ctx).loggedIn) { wm.cancelUniqueWork(PERIODIC); wm.cancelUniqueWork(SOON); return }
        wm.enqueueUniquePeriodicWork(PERIODIC, ExistingPeriodicWorkPolicy.KEEP,
            PeriodicWorkRequestBuilder<UpdateWorker>(6, TimeUnit.HOURS)
                .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.UNMETERED).build())
                .build())
    }

    /** A check soon: when the app is opened (at most hourly unless [force]), or [delaySec] after leaving it. */
    fun checkSoon(ctx: Context, delaySec: Long = 0, force: Boolean = false) {
        val p = Prefs(ctx)
        if (!p.loggedIn) return
        if (!force && System.currentTimeMillis() - p.updateChecked < 3_600_000) return
        WorkManager.getInstance(ctx).enqueueUniqueWork(SOON, ExistingWorkPolicy.REPLACE,
            OneTimeWorkRequestBuilder<UpdateWorker>()
                .setInitialDelay(delaySec, TimeUnit.SECONDS)
                .setConstraints(Constraints.Builder().setRequiredNetworkType(NetworkType.CONNECTED).build())
                .build())
    }

    fun clearDownloads(ctx: Context) {
        ctx.cacheDir.listFiles { f -> f.name.startsWith("update-") }?.forEach { it.delete() }
    }

    /** Hands the APK to Android's installer; the outcome arrives at UpdateReceiver. */
    fun install(ctx: Context, apk: File, rel: Api.Release) {
        val pi = ctx.packageManager.packageInstaller
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL).apply {
            setAppPackageName(ctx.packageName)
            setSize(apk.length())
            setInstallReason(PackageManager.INSTALL_REASON_USER)
            if (Build.VERSION.SDK_INT >= 31) setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_NOT_REQUIRED)
        }
        val id = pi.createSession(params)
        try {
            pi.openSession(id).use { s ->
                s.openWrite("PiFoto.apk", 0, apk.length()).use { out ->
                    apk.inputStream().use { it.copyTo(out) }
                    s.fsync(out)
                }
                val i = Intent(ctx, UpdateReceiver::class.java).setAction(ACTION_STATUS)
                    .putExtra("code", rel.code).putExtra("name", rel.name)
                // Mutable (explicit intent): the installer adds the outcome to it.
                val pending = PendingIntent.getBroadcast(ctx, id, i, PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_MUTABLE)
                s.commit(pending.intentSender)
            }
        } catch (e: Exception) {
            try { pi.abandonSession(id) } catch (_: Exception) {}
            throw e
        }
    }

    private fun channel(ctx: Context) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        if (nm.getNotificationChannel(CHANNEL) == null)
            nm.createNotificationChannel(NotificationChannel(CHANNEL, "App updates", NotificationManager.IMPORTANCE_DEFAULT))
    }

    /** Android wants a tap to install: [confirm] is its "update this app?" screen. */
    fun notifyReady(ctx: Context, name: String, confirm: Intent) {
        if (!Backup.canNotify(ctx)) return
        channel(ctx)
        val tap = PendingIntent.getActivity(ctx, NOTIF_READY, confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val n = Notification.Builder(ctx, CHANNEL)
            .setSmallIcon(R.drawable.ic_notif)
            .setContentTitle(if (name.isNotEmpty()) "PiFoto $name is ready" else "A PiFoto update is ready")
            .setContentText("Tap to install the update.")
            .setContentIntent(tap)
            .setAutoCancel(true)
            .build()
        ctx.getSystemService(NotificationManager::class.java).notify(NOTIF_READY, n)
    }

    /** Shown by the new version (only when the update came with notes). */
    fun notifyUpdated(ctx: Context, name: String, notes: String) {
        if (!Backup.canNotify(ctx)) return
        channel(ctx)
        val open = PendingIntent.getActivity(ctx, NOTIF_DONE,
            Intent(ctx, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        val n = Notification.Builder(ctx, CHANNEL)
            .setSmallIcon(R.drawable.ic_notif)
            .setContentTitle("PiFoto was updated to $name")
            .setContentText(notes)
            .setStyle(Notification.BigTextStyle().bigText(notes))
            .setContentIntent(open)
            .setAutoCancel(true)
            .build()
        ctx.getSystemService(NotificationManager::class.java).notify(NOTIF_DONE, n)
    }

    internal fun log(msg: String) = Log.i(TAG, msg)
}

class UpdateWorker(ctx: Context, params: WorkerParameters) : CoroutineWorker(ctx, params) {
    override suspend fun doWork(): Result = withContext(Dispatchers.IO) {
        val ctx = applicationContext
        val p = Prefs(ctx)
        if (!p.loggedIn || !Updater.busy.compareAndSet(false, true)) return@withContext Result.success()
        p.updateChecked = System.currentTimeMillis()
        val (have, haveName) = Updater.installed(ctx)
        try {
            // Already downloaded and waiting for the app to be left: install it without asking the Pi
            // again (the run right after leaving the app may get no network on newer Androids).
            var rel = p.updateRelease?.takeIf { it.code > have && it.code != p.updateFailed }
            var apk = rel?.let { File(ctx.cacheDir, "update-${it.code}.apk") }
            if (rel == null || apk == null || apk.length() != rel.size || Api.sha256(apk) != rel.sha256) {
                val session = p.session
                val base = Api.pickBase(p, session) // the checked home address, or the public https one
                rel = Api.latestApp(base, session, have, haveName)
                if (rel == null || rel.code <= have || rel.code == p.updateFailed) {
                    p.updateWaiting = false; p.updateNeedsTap = false; p.updateRelease = null
                    return@withContext Result.success()
                }
                apk = File(ctx.cacheDir, "update-${rel.code}.apk")
                if (apk.length() != rel.size || Api.sha256(apk) != rel.sha256) {
                    Updater.clearDownloads(ctx)
                    Updater.log("downloading ${rel.name} (${rel.code}) from $base")
                    Api.downloadApp(base, rel, apk)
                }
                p.updateRelease = rel
            }
            // Installing replaces the running app: not while someone uses it or a backup uploads.
            // Except when Android will ask first anyway: then the person here gets its screen.
            if (!Updater.askNow(p) && (Updater.shown > 0 || Backup.running.get())) {
                p.updateWaiting = true
                Updater.log("${rel.name} waits until the app isn't in use")
                if (Updater.shown == 0) Updater.checkSoon(ctx, delaySec = 600, force = true)
                return@withContext Result.success()
            }
            p.updateWaiting = false
            p.updateName = rel.name
            p.updateNotes = rel.notes
            Updater.log("installing ${rel.name} (${rel.code})")
            Updater.install(ctx, apk, rel)
        } catch (e: Api.AuthException) {
            // logged out elsewhere: the backup code tells the person
        } catch (e: IOException) {
            Updater.log("update check failed: ${e.message}")
        } catch (e: Exception) {
            Updater.log("update failed: $e")
        } finally {
            Updater.busy.set(false)
        }
        Result.success()
    }
}

/** The installer's answer (ACTION_STATUS), and the new version's first start (MY_PACKAGE_REPLACED). */
class UpdateReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        when (intent.action) {
            Updater.ACTION_STATUS -> {
                val status = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)
                val name = intent.getStringExtra("name") ?: ""
                when (status) {
                    PackageInstaller.STATUS_PENDING_USER_ACTION -> {
                        val confirm = if (Build.VERSION.SDK_INT >= 33) intent.getParcelableExtra(Intent.EXTRA_INTENT, Intent::class.java)
                            else @Suppress("DEPRECATION") intent.getParcelableExtra(Intent.EXTRA_INTENT)
                        Updater.log("$name needs a tap to install")
                        Prefs(ctx).updateNeedsTap = true
                        if (confirm == null) return
                        // The app is on screen: show Android's install screen right away (the first time it
                        // also offers "allow from this source", after which updates install by themselves).
                        // Otherwise a notification, and the screen comes the next time the app is opened.
                        if (Updater.shown > 0) {
                            try { ctx.startActivity(confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)); return } catch (_: Exception) {}
                        }
                        Updater.notifyReady(ctx, name, confirm)
                    }
                    PackageInstaller.STATUS_SUCCESS -> Updater.log("$name installed")
                    else -> {
                        Updater.log("installing $name failed: $status ${intent.getStringExtra(PackageInstaller.EXTRA_STATUS_MESSAGE)}")
                        if (status == PackageInstaller.STATUS_FAILURE_ABORTED) Prefs(ctx).updateDeclined = System.currentTimeMillis()
                        // Never going to work (another signing key, too old an Android): don't retry this version.
                        if (status == PackageInstaller.STATUS_FAILURE_INCOMPATIBLE || status == PackageInstaller.STATUS_FAILURE_CONFLICT ||
                            status == PackageInstaller.STATUS_FAILURE_INVALID) Prefs(ctx).updateFailed = intent.getLongExtra("code", 0)
                        Prefs(ctx).updateName = ""
                    }
                }
            }
            Intent.ACTION_MY_PACKAGE_REPLACED -> {
                val p = Prefs(ctx)
                ctx.getSystemService(NotificationManager::class.java).cancel(Updater.NOTIF_READY)
                Updater.clearDownloads(ctx)
                p.updateWaiting = false; p.updateNeedsTap = false; p.updateRelease = null
                Backup.schedule(ctx)
                Updater.schedule(ctx)
                val name = p.updateName
                if (name.isNotEmpty() && p.updateNotes.isNotBlank()) Updater.notifyUpdated(ctx, name, p.updateNotes.trim())
                p.updateName = ""; p.updateNotes = ""
            }
        }
    }
}
