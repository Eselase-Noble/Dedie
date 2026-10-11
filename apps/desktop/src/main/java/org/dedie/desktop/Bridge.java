package org.dedie.desktop;

import java.io.IOException;
import java.io.InputStream;
import java.net.CookieHandler;
import java.net.URI;
import java.net.http.HttpClient;
import java.net.http.HttpRequest;
import java.net.http.HttpResponse;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.StandardCopyOption;
import java.time.Duration;
import java.util.HashMap;
import java.util.List;
import java.util.Map;

import javafx.application.HostServices;
import javafx.application.Platform;
import javafx.scene.control.Alert;
import javafx.scene.control.ButtonType;
import javafx.scene.input.Clipboard;
import javafx.scene.input.ClipboardContent;
import javafx.stage.FileChooser;
import javafx.stage.Stage;

/**
 * What the interface can ask the desktop application to do for it.
 *
 * <p>The embedded browser has no download manager, no clipboard access and
 * no way to show a notification outside its window, so the pages hand
 * those jobs to this object, which they find as {@code window.dedieDesktop}.
 * Its public methods are the whole of what a page can reach.
 */
public final class Bridge {

    private final Stage stage;
    private final HostServices host;
    private final String baseUrl;
    private final Notifier notifier;

    Bridge(Stage stage, HostServices host, String baseUrl, Notifier notifier) {
        this.stage = stage;
        this.host = host;
        this.notifier = notifier;
        this.baseUrl = baseUrl.endsWith("/") ? baseUrl.substring(0, baseUrl.length() - 1) : baseUrl;
    }

    /** Saves text the page produced, such as an exported result, to a file the user picks. */
    public void save(String name, String text) {
        Path target = choose(name);
        if (target == null) {
            return;
        }
        try {
            Files.writeString(target, text, StandardCharsets.UTF_8);
        } catch (IOException e) {
            alert(Alert.AlertType.ERROR, "The file could not be saved: " + e.getMessage());
        }
    }

    /**
     * Downloads a file from the engine, such as a backup, to a file the
     * user picks. Only paths on the local engine are accepted.
     */
    public void download(String path, String suggestedName) {
        if (path == null || !path.startsWith("/") || path.startsWith("//")) {
            return;
        }
        URI uri = URI.create(baseUrl + path);
        Path target = choose(suggestedName == null || suggestedName.isBlank() ? "download" : suggestedName);
        if (target == null) {
            return;
        }
        // The session cookie is HttpOnly, so the page cannot read it; the
        // browser's own cookie store hands it over for this address.
        String cookies = cookiesFor(uri);
        Thread worker = new Thread(() -> {
            Path partial = target.resolveSibling(target.getFileName() + ".part");
            try {
                HttpRequest.Builder request = HttpRequest.newBuilder(uri).timeout(Duration.ofHours(6)).GET();
                if (!cookies.isEmpty()) {
                    request.header("Cookie", cookies);
                }
                HttpResponse<InputStream> response = HttpClient.newHttpClient()
                        .send(request.build(), HttpResponse.BodyHandlers.ofInputStream());
                try (InputStream body = response.body()) {
                    if (response.statusCode() != 200) {
                        throw new IOException("the engine answered " + response.statusCode());
                    }
                    Files.copy(body, partial, StandardCopyOption.REPLACE_EXISTING);
                }
                Files.move(partial, target, StandardCopyOption.REPLACE_EXISTING);
                Platform.runLater(() -> alert(Alert.AlertType.INFORMATION, "Saved " + target));
            } catch (IOException | InterruptedException e) {
                try {
                    Files.deleteIfExists(partial);
                } catch (IOException ignored) {
                    // Nothing more can be done about a leftover partial file.
                }
                Platform.runLater(() -> alert(Alert.AlertType.ERROR, "The download failed: " + e.getMessage()));
            }
        }, "dedie-download");
        worker.setDaemon(true);
        worker.start();
    }

    /** Puts text on the system clipboard. */
    public void copy(String text) {
        ClipboardContent content = new ClipboardContent();
        content.putString(text == null ? "" : text);
        Clipboard.getSystemClipboard().setContent(content);
    }

    /** Opens a web address in the system browser. */
    public void openExternal(String url) {
        if (url != null && (url.startsWith("https://") || url.startsWith("http://"))) {
            host.showDocument(url);
        }
    }

    /**
     * Shows a notification by the system tray for something the user is
     * not looking at: the window is behind another or minimized. When the
     * window is in front the page's own notification is enough, and where
     * there is no tray it is all there is. Clicking the notification
     * brings the window forward on the page named by link, a path on the
     * local engine.
     */
    public void notify(String title, String body, String link) {
        if (title == null || title.isBlank() || (stage.isFocused() && !stage.isIconified())) {
            return;
        }
        boolean local = link != null && link.startsWith("/") && !link.startsWith("//");
        notifier.show(title, body == null ? "" : body, local ? link : null);
    }

    private Path choose(String name) {
        FileChooser chooser = new FileChooser();
        chooser.setTitle("Save as");
        chooser.setInitialFileName(name);
        Path downloads = Path.of(System.getProperty("user.home"), "Downloads");
        if (Files.isDirectory(downloads)) {
            chooser.setInitialDirectory(downloads.toFile());
        }
        java.io.File file = chooser.showSaveDialog(stage);
        return file == null ? null : file.toPath();
    }

    private static String cookiesFor(URI uri) {
        try {
            CookieHandler handler = CookieHandler.getDefault();
            if (handler == null) {
                return "";
            }
            Map<String, List<String>> headers = handler.get(uri, new HashMap<>());
            List<String> values = headers.get("Cookie");
            return values == null ? "" : String.join("; ", values);
        } catch (IOException e) {
            return "";
        }
    }

    private void alert(Alert.AlertType type, String message) {
        Alert alert = new Alert(type, message, ButtonType.OK);
        alert.initOwner(stage);
        alert.setHeaderText(null);
        alert.showAndWait();
    }
}
