import java.util.Properties

plugins {
    alias(libs.plugins.android.application)
}

// Release signing key: kept outside the project (~/.config/pifoto, mode 600), so it never travels
// with the source. It is the key the APKs already on phones were signed with (the PC's former debug
// key, now with its own password), so installed apps update in place; a different key would make
// Android refuse the update. Debug and devtest builds keep using the debug key.
val releaseSigningFile = File(System.getProperty("user.home"), ".config/pifoto/release-signing.properties")
val releaseSigning = Properties().apply {
    if (releaseSigningFile.isFile) releaseSigningFile.inputStream().use { load(it) }
}
val releaseSigningProblem: String? = when {
    !releaseSigningFile.isFile -> "$releaseSigningFile is missing"
    else -> listOf("storeFile", "storePassword", "keyAlias", "keyPassword")
        .filter { releaseSigning.getProperty(it).isNullOrBlank() }
        .takeIf { it.isNotEmpty() }?.let { "$releaseSigningFile has no ${it.joinToString()}" }
}

// The Pi's public address (Tailscale Funnel), which the app uses away from home until the server
// tells it its own (-remote-url). It belongs to one particular Pi, so it is not in the source:
// put `pifoto.publicServer=https://<pi>.<tailnet>.ts.net` in android/local.properties.
val localProperties = Properties().apply {
    rootProject.file("local.properties").takeIf { it.isFile }?.inputStream()?.use { load(it) }
}
val publicServer = (localProperties.getProperty("pifoto.publicServer") ?: "").trim().trimEnd('/')
require(publicServer.isEmpty() || Regex("https://[A-Za-z0-9.-]+(:[0-9]+)?").matches(publicServer)) {
    "pifoto.publicServer in local.properties must look like https://name.example.ts.net (got '$publicServer')"
}

android {
    namespace = "app.pifoto"
    compileSdk {
        version = release(36) {
            minorApiLevel = 1
        }
    }

    defaultConfig {
        applicationId = "app.pifoto"
        minSdk = 30
        targetSdk = 36
        versionCode = 8
        versionName = "1.4.2"
        manifestPlaceholders["appLabel"] = "PiFoto"
        buildConfigField("String", "PUBLIC_SERVER", "\"$publicServer\"")
    }
    buildFeatures {
        buildConfig = true
    }

    signingConfigs {
        create("release") {
            if (releaseSigningProblem == null) {
                storeFile = file(releaseSigning.getProperty("storeFile"))
                storePassword = releaseSigning.getProperty("storePassword")
                keyAlias = releaseSigning.getProperty("keyAlias")
                keyPassword = releaseSigning.getProperty("keyPassword")
            }
        }
    }

    buildTypes {
        // The APK that is published (assembleRelease): signed with the release key, not debuggable.
        release {
            isDebuggable = false
            signingConfig = signingConfigs.getByName("release")
            optimization {
                enable = false
            }
        }
        // Debug builds are debuggable and signed with the PC's debug keystore (well-known password):
        // their own package, so one can never replace the published app on a phone.
        debug {
            applicationIdSuffix = ".debug"
            versionNameSuffix = "-debug"
            manifestPlaceholders["appLabel"] = "PiFoto Debug"
        }
        // Side-by-side test build ("PiFoto Dev", app.pifoto.devtest) for trying changes on a real
        // device without touching the installed PiFoto. Its manifest drops photo access, so it can
        // never back anything up, and it may use plain HTTP anywhere (src/devtest/res/xml). Build
        // with assembleDevtest.
        create("devtest") {
            initWith(getByName("debug"))
            applicationIdSuffix = ".devtest"
            versionNameSuffix = "-devtest"
            manifestPlaceholders["appLabel"] = "PiFoto Dev"
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

// Without the key only release builds fail, with a message saying why; debug and devtest don't need it.
releaseSigningProblem?.let { problem ->
    val msg = "Release signing: $problem. It needs storeFile, storePassword, keyAlias and keyPassword " +
        "for the key the published PiFoto APKs are signed with (otherwise installed apps can't update)."
    tasks.configureEach {
        if (name == "preReleaseBuild") doFirst { throw GradleException(msg) }
    }
}

// A release without the public address would not work away from home until its first login at home.
if (publicServer.isEmpty()) {
    tasks.configureEach {
        if (name == "preReleaseBuild") doFirst {
            throw GradleException("pifoto.publicServer is missing from android/local.properties (the Pi's https:// address)")
        }
    }
}

dependencies {
    implementation(libs.androidx.core.ktx)
    implementation(libs.androidx.work)
}
