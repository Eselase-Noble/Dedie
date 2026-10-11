package org.dedie.desktop;

import java.nio.file.Files;
import java.nio.file.Path;
import java.util.prefs.Preferences;

import javafx.application.Application;
import javafx.application.Platform;
import javafx.concurrent.Worker;
import javafx.geometry.Insets;
import javafx.geometry.Pos;
import javafx.scene.Scene;
import javafx.scene.control.Alert;
import javafx.scene.control.Button;
import javafx.scene.control.ButtonType;
import javafx.scene.control.Label;
import javafx.scene.control.Menu;
import javafx.scene.control.MenuBar;
import javafx.scene.control.MenuItem;
import javafx.scene.control.ProgressIndicator;
import javafx.scene.control.SeparatorMenuItem;
import javafx.scene.input.KeyCombination;
import javafx.scene.layout.BorderPane;
import javafx.scene.layout.StackPane;
import javafx.scene.layout.VBox;
import javafx.scene.web.WebEngine;
import javafx.scene.web.WebHistory;
import javafx.scene.web.WebView;
import javafx.stage.Stage;
import netscape.javascript.JSObject;

/**
 * The application window: a status screen while the engine starts, then the
 * Dedie interface under a native menu bar.
 */
public final class DedieApp extends Application {

    private static final String WEBSITE = "https://eselase-noble.github.io/Dedie/";

    private final Engine engine = new Engine();
    private final Preferences prefs = Preferences.userNodeForPackage(DedieApp.class);
    private final BorderPane frame = new BorderPane();
    private final StackPane root = new StackPane();
    private Stage stage;
    private WebView view;
    private String url;
    // Held here because the page only keeps a weak reference to it.
    private Bridge bridge;

    @Override
    public void start(Stage primaryStage) {
        stage = primaryStage;
        root.setStyle("-fx-background-color: white;");
        frame.setCenter(root);
        frame.setTop(menuBar());
        showStatus("Starting Dedie…", true);

        stage.setTitle("Dedie");
        stage.setScene(new Scene(frame, prefs.getDouble("width", 1360), prefs.getDouble("height", 860)));
        stage.setMinWidth(900);
        stage.setMinHeight(600);
        // Reopen where the window was left, unless that screen is gone.
        double x = prefs.getDouble("x", Double.NaN);
        double y = prefs.getDouble("y", Double.NaN);
        if (!Double.isNaN(x) && !Double.isNaN(y)
                && !javafx.stage.Screen.getScreensForRectangle(x, y, 200, 200).isEmpty()) {
            stage.setX(x);
            stage.setY(y);
        }
        stage.setMaximized(prefs.getBoolean("maximized", false));
        stage.show();
        startEngine();
    }

    @Override
    public void stop() {
        if (!stage.isMaximized() && !stage.isFullScreen()) {
            prefs.putDouble("width", stage.getScene().getWidth());
            prefs.putDouble("height", stage.getScene().getHeight());
            prefs.putDouble("x", stage.getX());
            prefs.putDouble("y", stage.getY());
        }
        prefs.putBoolean("maximized", stage.isMaximized());
        if (view != null) {
            prefs.putDouble("zoom", view.getZoom());
        }
        engine.stop();
    }

    private void startEngine() {
        showStatus("Starting Dedie…", true);
        // The engine is started off the interface thread; the window stays
        // responsive while it opens its database and binds a port.
        engine.start().whenComplete((address, error) -> Platform.runLater(() -> {
            if (error != null) {
                showFailure(error.getMessage());
                return;
            }
            url = address;
            showInterface();
            engine.stoppedUnexpectedly().thenRun(() -> Platform.runLater(() ->
                    showFailure("The Dedie engine stopped unexpectedly. Its log is in " + Engine.logFile() + ".")));
        }));
    }

    // --- menus ---

    private MenuBar menuBar() {
        Menu file = new Menu("File");
        file.getItems().addAll(
                item("Open in Browser", null, () -> open(url)),
                item("Reload", "Shortcut+R", () -> script("window.location.reload()")),
                new SeparatorMenuItem(),
                item("Quit", "Shortcut+Q", Platform::exit));

        Menu go = new Menu("Go");
        go.getItems().addAll(
                item("Back", "Alt+Left", () -> history(-1)),
                item("Forward", "Alt+Right", () -> history(1)),
                new SeparatorMenuItem(),
                item("Overview", "Shortcut+1", () -> page("")),
                item("Connections", "Shortcut+2", () -> page("connections/")),
                item("Query Browser", "Shortcut+3", () -> page("query/")),
                item("Migrations", "Shortcut+4", () -> page("migrations/")),
                item("Backups", "Shortcut+5", () -> page("backups/")),
                item("Monitoring", "Shortcut+6", () -> page("monitoring/")),
                item("Server Activity", "Shortcut+7", () -> page("server/")),
                item("Database Users", "Shortcut+8", () -> page("dbusers/")),
                new SeparatorMenuItem(),
                item("Account", null, () -> page("account/")));

        Menu viewMenu = new Menu("View");
        viewMenu.getItems().addAll(
                item("Zoom In", "Shortcut+Plus", () -> zoom(0.1)),
                item("Zoom Out", "Shortcut+Minus", () -> zoom(-0.1)),
                item("Actual Size", "Shortcut+0", () -> zoom(0)),
                new SeparatorMenuItem(),
                item("Full Screen", "F11", () -> stage.setFullScreen(!stage.isFullScreen())));

        Menu help = new Menu("Help");
        help.getItems().addAll(
                item("Dedie Website", null, () -> open(WEBSITE)),
                item("Show Log", null, () -> open(Engine.logFile().toUri().toString())),
                item("Open Data Folder", null, () -> open(Engine.logFile().getParent().toUri().toString())),
                new SeparatorMenuItem(),
                item("About Dedie", null, this::about));

        MenuBar bar = new MenuBar(file, go, viewMenu, help);
        // On macOS the menus belong at the top of the screen.
        bar.setUseSystemMenuBar(true);
        return bar;
    }

    private static MenuItem item(String text, String shortcut, Runnable action) {
        MenuItem item = new MenuItem(text);
        if (shortcut != null) {
            item.setAccelerator(KeyCombination.keyCombination(shortcut));
        }
        item.setOnAction(e -> action.run());
        return item;
    }

    private void open(String address) {
        if (address != null) {
            getHostServices().showDocument(address);
        }
    }

    private void script(String code) {
        if (view != null) {
            view.getEngine().executeScript(code);
        }
    }

    private void page(String path) {
        if (view != null && url != null) {
            view.getEngine().load(url + path);
        }
    }

    private void history(int offset) {
        if (view == null) {
            return;
        }
        WebHistory history = view.getEngine().getHistory();
        int target = history.getCurrentIndex() + offset;
        if (target >= 0 && target < history.getEntries().size()) {
            history.go(offset);
        }
    }

    /** Changes the zoom by a step, or resets it when the step is zero. */
    private void zoom(double step) {
        if (view != null) {
            view.setZoom(step == 0 ? 1 : Math.max(0.5, Math.min(2.5, view.getZoom() + step)));
        }
    }

    private void about() {
        String version = getClass().getPackage().getImplementationVersion();
        Alert alert = new Alert(Alert.AlertType.INFORMATION,
                "Database migration, backup and administration.\n\n"
                        + "This window shows the Dedie engine running on this computer only; nothing outside it can connect.\n\n"
                        + "Data: " + Engine.logFile().getParent() + "\nEngine: " + (url == null ? "not running" : url),
                ButtonType.OK);
        alert.initOwner(stage);
        alert.setTitle("About Dedie");
        alert.setHeaderText("Dedie" + (version == null ? "" : " " + version));
        alert.showAndWait();
    }

    // --- content ---

    private void showStatus(String message, boolean busy) {
        Label label = new Label(message);
        label.setStyle("-fx-font-size: 15px; -fx-text-fill: #334155;");
        label.setWrapText(true);
        label.setMaxWidth(560);
        VBox box = new VBox(18);
        box.setAlignment(Pos.CENTER);
        box.setPadding(new Insets(40));
        if (busy) {
            box.getChildren().add(new ProgressIndicator());
        }
        box.getChildren().add(label);
        root.getChildren().setAll(box);
    }

    private void showFailure(String message) {
        view = null;
        showStatus(message, false);
        Button retry = new Button("Start again");
        retry.setOnAction(e -> startEngine());
        Button log = new Button("Show log");
        log.setOnAction(e -> open(Engine.logFile().toUri().toString()));
        Button quit = new Button("Quit");
        quit.setOnAction(e -> Platform.exit());
        javafx.scene.layout.HBox buttons = new javafx.scene.layout.HBox(10, retry, log, quit);
        buttons.setAlignment(Pos.CENTER);
        ((VBox) root.getChildren().get(0)).getChildren().add(buttons);
    }

    private void showInterface() {
        view = new WebView();
        view.setContextMenuEnabled(false);
        view.setZoom(prefs.getDouble("zoom", 1));
        WebEngine web = view.getEngine();
        bridge = new Bridge(stage, getHostServices(), url);

        // Keeps the browser-side preferences (open tabs, query history)
        // between runs, alongside the rest of Dedie's data.
        Path profile = Engine.logFile().getParent().resolve("desktop");
        try {
            Files.createDirectories(profile);
            web.setUserDataDirectory(profile.toFile());
        } catch (java.io.IOException e) {
            // Without a profile directory the preferences last one session.
        }

        // The window only ever shows the local engine. Anything else, such
        // as a link in a result, opens in the system browser instead.
        web.locationProperty().addListener((observable, previous, location) -> {
            if (location != null && !location.startsWith(url) && !location.startsWith("about:")) {
                Platform.runLater(() -> web.load(previous != null ? previous : url));
                open(location);
            }
        });
        web.setCreatePopupHandler(features -> null);

        web.setConfirmHandler(message -> {
            Alert alert = new Alert(Alert.AlertType.CONFIRMATION, message, ButtonType.OK, ButtonType.CANCEL);
            alert.initOwner(stage);
            alert.setHeaderText(null);
            return alert.showAndWait().filter(ButtonType.OK::equals).isPresent();
        });
        web.setOnAlert(event -> {
            Alert alert = new Alert(Alert.AlertType.INFORMATION, event.getData(), ButtonType.OK);
            alert.initOwner(stage);
            alert.setHeaderText(null);
            alert.showAndWait();
        });

        web.titleProperty().addListener((observable, previous, title) ->
                stage.setTitle(title == null || title.isBlank() ? "Dedie" : title));

        web.getLoadWorker().stateProperty().addListener((observable, previous, state) -> {
            if (state == Worker.State.FAILED) {
                showFailure("The interface could not be loaded from " + url + ".");
            } else if (state == Worker.State.SUCCEEDED) {
                // Every new document gets the bridge; the pages use it for
                // downloads and the clipboard, which this browser lacks.
                ((JSObject) web.executeScript("window")).setMember("dedieDesktop", bridge);
                if (Boolean.getBoolean("dedie.smoke")) {
                    smokeTest(web);
                }
            }
        });

        root.getChildren().setAll(view);
        web.load(url);
    }

    /**
     * With {@code -Ddedie.smoke=true} the application waits for the page to
     * render, prints what it sees and exits. Used to check a build on a
     * machine without anyone watching the window.
     */
    private void smokeTest(WebEngine web) {
        Thread wait = new Thread(() -> {
            try {
                Thread.sleep(4000);
            } catch (InterruptedException ignored) {
                return;
            }
            Platform.runLater(() -> {
                Object text = web.executeScript("document.title + ' | ' + location.pathname + ' | bridge:' + "
                        + "(typeof window.dedieDesktop) + ' | ' + "
                        + "document.body.innerText.replace(/\\s+/g, ' ').slice(0, 160)");
                System.out.println("SMOKE " + text);
                Platform.exit();
            });
        }, "dedie-smoke-test");
        wait.setDaemon(true);
        wait.start();
    }
}
