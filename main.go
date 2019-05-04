package main

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/pkg/errors"
)

func main() {
	err := run()
	if err != nil {
		switch err.(type) {
		case *emptyArgError:
			usage()
		case *badArgError:
			fmt.Println("error: " + err.Error())
			usage()
		default:
			fmt.Println("error: " + err.Error())
		}
		os.Exit(1)
	}
}

func run() error {
	nonInteractive := flag.Bool("n", false,
		"Non-interactive mode. Fail if shh would prompt for the password")
	flag.Parse()

	arg, tail := parseArg(flag.Args())
	if arg == "" || arg == "help" {
		return &emptyArgError{}
	}

	// Enforce that a .shh file exists for anything for most commands
	switch arg {
	case "init", "gen-keys", "serve", "version": // Do nothing
	default:
		_, err := findShhRecursive(".shh")
		if os.IsNotExist(err) {
			return errors.New("missing .shh, run `shh init`")
		}
		if err != nil {
			return err
		}
	}
	switch arg {
	case "init":
		if tail != nil {
			return fmt.Errorf("unknown args: %v", tail)
		}
		return initShh()
	case "gen-keys":
		return genKeys(tail)
	case "get":
		return get(*nonInteractive, tail)
	case "set":
		return set(tail)
	case "del":
		return del(tail)
	case "edit":
		return edit(*nonInteractive, tail)
	case "allow":
		return allow(*nonInteractive, tail)
	case "deny":
		return deny(tail)
	case "add-user":
		return addUser(tail)
	case "rm-user":
		return rmUser(tail)
	case "rotate":
		return rotate(tail)
	case "serve":
		return serve(tail)
	case "login":
		return login(tail)
	case "show":
		return show(tail)
	case "version":
		fmt.Println("1.1.5")
		return nil
	default:
		return &badArgError{Arg: arg}
	}
}

// parseArg splits the arguments into a head and tail.
func parseArg(args []string) (string, []string) {
	switch len(args) {
	case 0:
		return "", nil
	case 1:
		return args[0], nil
	default:
		return args[0], args[1:]
	}
}

// genKeys for self in ~/.config/shh.
func genKeys(args []string) error {
	if len(args) != 0 {
		return errors.New("bad args: expected none")
	}
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}
	_, err = configFromPath(configPath)
	if err == nil {
		return errors.New("keys exist at ~/.config/shh, run `shh rotate` to change keys")
	}
	if _, err = createUser(configPath); err != nil {
		return err
	}
	backupReminder(true)
	return nil
}

// initShh creates your project file ".shh". If the project file already
// exists or if keys have not been generated, initShh reports an error.
func initShh() error {
	if _, err := os.Stat(".shh"); err == nil {
		return errors.New(".shh already exists")
	}
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}
	user, err := getUser(configPath)
	if err != nil {
		return errors.Wrap(err, "get user")
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return errors.Wrap(err, "shh from path")
	}
	shh.Keys[user.Username] = user.Keys.PublicKeyBlock
	return shh.EncodeToFile()
}

// TODO enforce 600 permissions on id_rsa file and .shh when any command is run

// get a secret value by name.
func get(nonInteractive bool, args []string) error {
	if len(args) != 1 {
		return errors.New("bad args: expected `get $name`")
	}
	secretName := args[0]
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}
	user, err := getUser(configPath)
	if err != nil {
		return errors.Wrap(err, "get user")
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	secrets, err := shh.GetSecretsForUser(secretName, user.Username)
	if err != nil {
		return err
	}
	if nonInteractive {
		user.Password, err = requestPasswordFromServer(user.Port, false)
		if err != nil {
			return err
		}
	} else {
		user.Password, err = requestPassword(user.Port, defaultPasswordPrompt)
		if err != nil {
			return err
		}
	}
	keys, err := getKeys(configPath, user.Password)
	if err != nil {
		return err
	}
	for _, secret := range secrets {
		// Decrypt the AES key using the private key
		aesKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader,
			keys.PrivateKey, []byte(secret.AESKey), nil)
		if err != nil {
			return errors.Wrap(err, "decrypt secret")
		}

		// Use the decrypted AES key to decrypt the secret
		aesBlock, err := aes.NewCipher(aesKey)
		if err != nil {
			return err
		}

		if len(secret.Encrypted) < aes.BlockSize {
			return errors.New("encrypted secret too short")
		}
		ciphertext := []byte(secret.Encrypted)
		iv := ciphertext[:aes.BlockSize]
		ciphertext = ciphertext[aes.BlockSize:]
		stream := cipher.NewCFBDecrypter(aesBlock, iv)
		plaintext := make([]byte, len(ciphertext))
		stream.XORKeyStream(plaintext, []byte(ciphertext))
		fmt.Print(string(plaintext))
	}
	return nil
}

// set a secret value.
func set(args []string) error {
	if len(args) != 2 {
		return errors.New("bad args: expected `set $name $val`")
	}
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}
	user, err := getUser(configPath)
	if err != nil {
		return err
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	if _, exist := shh.Secrets[user.Username]; !exist {
		shh.Secrets[user.Username] = map[string]secret{}
	}
	key := args[0]
	plaintext := args[1]

	// Encrypt content for each user with access to the secret
	for username, secrets := range shh.Secrets {
		if username != user.Username {
			if _, ok := secrets[key]; !ok {
				continue
			}
		}

		// Generate an AES key to encrypt the data. We use AES-256
		// which requires a 32-byte key
		aesKey := make([]byte, 32)
		if _, err := rand.Read(aesKey); err != nil {
			return err
		}
		aesBlock, err := aes.NewCipher(aesKey)
		if err != nil {
			return err
		}

		// Encrypt the secret using the new AES key
		encrypted := make([]byte, aes.BlockSize+len(plaintext))
		iv := encrypted[:aes.BlockSize]
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return errors.Wrap(err, "read iv")
		}
		stream := cipher.NewCFBEncrypter(aesBlock, iv)
		stream.XORKeyStream(encrypted[aes.BlockSize:], []byte(plaintext))

		// Encrypt the AES key using the public key
		pubKey, err := x509.ParsePKCS1PublicKey(shh.Keys[username].Bytes)
		if err != nil {
			return errors.Wrap(err, "parse public key")
		}
		encryptedAES, err := rsa.EncryptOAEP(sha256.New(), rand.Reader,
			pubKey, aesKey, nil)
		if err != nil {
			return errors.Wrap(err, "reencrypt secret")
		}

		// We base64 encode all encrypted data before passing it into
		// the .shh file
		sec := secret{
			AESKey:    base64.StdEncoding.EncodeToString(encryptedAES),
			Encrypted: base64.StdEncoding.EncodeToString(encrypted),
		}
		shh.Secrets[username][key] = sec
	}
	return shh.EncodeToFile()
}

// del deletes a secret for all users.
func del(args []string) error {
	if len(args) != 1 {
		return errors.New("bad args: expected `del $secret`")
	}
	secret := args[0]
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}
	user, err := getUser(configPath)
	if err != nil {
		return err
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	secrets, err := shh.GetSecretsForUser(secret, user.Username)
	if err != nil {
		return err
	}
	userSecrets := shh.Secrets[user.Username]
	for key := range secrets {
		delete(userSecrets, key)
	}
	if len(userSecrets) == 0 {
		delete(shh.Secrets, user.Username)
	}
	err = shh.EncodeToFile()
	return errors.Wrap(err, "encode to path")
}

// allow a user to access a secret. You must have access yourself.
//
// TODO allow all using "$user *" syntax.
func allow(nonInteractive bool, args []string) error {
	if len(args) != 2 {
		return errors.New("bad args: expected `allow $user $secret`")
	}
	username := username(args[0])
	secretKey := args[1]
	configPath, err := getConfigPath()
	if err != nil {
		return errors.Wrap(err, "get config path")
	}
	user, err := getUser(configPath)
	if err != nil {
		return errors.Wrap(err, "get user")
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	block, exist := shh.Keys[username]
	if !exist {
		return fmt.Errorf("%q is not a user in the project. try `shh add-user %s $PUBKEY`", username, username)
	}
	pubKey, err := x509.ParsePKCS1PublicKey(block.Bytes)
	if err != nil {
		return errors.Wrap(err, "parse public key")
	}

	// Decrypt all matching secrets
	if nonInteractive {
		user.Password, err = requestPasswordFromServer(user.Port, false)
		if err != nil {
			return err
		}
	} else {
		user.Password, err = requestPassword(user.Port, defaultPasswordPrompt)
		if err != nil {
			return err
		}
	}
	keys, err := getKeys(configPath, user.Password)
	if err != nil {
		return errors.Wrap(err, "get keys")
	}
	secrets, err := shh.GetSecretsForUser(secretKey, user.Username)
	if err != nil {
		return err
	}
	if len(secrets) == 0 {
		return errors.New("no matching secrets which you can access")
	}
	if _, exist := shh.Secrets[username]; !exist {
		shh.Secrets[username] = map[string]secret{}
	}
	for key, sec := range secrets {
		// Decrypt AES key using personal RSA key
		aesKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader,
			keys.PrivateKey, []byte(sec.AESKey), nil)
		if err != nil {
			return errors.Wrap(err, "decrypt secret")
		}
		aesBlock, err := aes.NewCipher(aesKey)
		if err != nil {
			return err
		}
		ciphertext := []byte(sec.Encrypted)
		iv := ciphertext[:aes.BlockSize]
		ciphertext = ciphertext[aes.BlockSize:]
		stream := cipher.NewCFBDecrypter(aesBlock, iv)
		plaintext := make([]byte, len(ciphertext))
		stream.XORKeyStream(plaintext, []byte(ciphertext))

		// Generate an AES key to encrypt the data. We use AES-256
		// which requires a 32-byte key
		aesKey = make([]byte, 32)
		if _, err := rand.Read(aesKey); err != nil {
			return err
		}
		aesBlock, err = aes.NewCipher(aesKey)
		if err != nil {
			return err
		}

		// Encrypt the secret using the new AES key
		encrypted := make([]byte, aes.BlockSize+len(plaintext))
		iv = encrypted[:aes.BlockSize]
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return errors.Wrap(err, "read iv")
		}
		stream = cipher.NewCFBEncrypter(aesBlock, iv)
		stream.XORKeyStream(encrypted[aes.BlockSize:], []byte(plaintext))

		// Encrypt the AES key using the public key
		encryptedAES, err := rsa.EncryptOAEP(sha256.New(), rand.Reader,
			pubKey, aesKey, nil)
		if err != nil {
			return errors.Wrap(err, "reencrypt secret")
		}

		// We base64 encode all encrypted data before passing it into
		// the .shh file
		sec := secret{
			AESKey:    base64.StdEncoding.EncodeToString(encryptedAES),
			Encrypted: base64.StdEncoding.EncodeToString(encrypted),
		}

		// Add encrypted data and key to .shh
		shh.Secrets[username][key] = sec
	}
	return shh.EncodeToFile()
}

// deny a user from accessing secrets.
func deny(args []string) error {
	if len(args) > 2 {
		return errors.New("bad args: expected `deny $user [$secret]`")
	}
	var secretKey string
	if len(args) == 1 {
		secretKey = "*"
	} else {
		secretKey = args[1]
	}
	username := username(args[0])
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	secrets, err := shh.GetSecretsForUser(secretKey, username)
	if err != nil {
		return errors.Wrap(err, "get secrets for user")
	}
	userSecrets := shh.Secrets[username]
	for key := range secrets {
		delete(userSecrets, key)
	}
	if len(userSecrets) == 0 {
		delete(shh.Secrets, username)
	}
	return shh.EncodeToFile()
}

// show users and secrets which they can access.
func show(args []string) error {
	if len(args) > 1 {
		return errors.New("bad args: expected `show [$user]`")
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return showAll(shh)
	}
	return showUser(shh, username(args[0]))
}

// showAll users and secrets alongside a summary.
func showAll(shh *shh) error {
	secrets := shh.AllSecrets()
	fmt.Println("====== SUMMARY ======")
	fmt.Printf("%d users\n", len(shh.Keys))
	fmt.Printf("%d secrets\n", len(secrets))
	fmt.Printf("\n")
	fmt.Printf("======= USERS =======")
	usernames := []string{}
	for uname := range shh.Keys {
		usernames = append(usernames, string(uname))
	}
	sort.Strings(usernames)
	for _, uname := range usernames {
		userSecrets := shh.Secrets[username(uname)]
		fmt.Printf("\n%s (%d secrets)\n", uname, len(userSecrets))
		for secretName := range userSecrets {
			fmt.Printf("> %s\n", secretName)
		}
	}
	return nil
}

// showUser secrets.
func showUser(shh *shh, username username) error {
	secrets, ok := shh.Secrets[username]
	if !ok {
		return fmt.Errorf("unknown user: %s", username)
	}
	fmt.Printf("%d secrets\n", len(secrets))
	for secretName := range secrets {
		fmt.Printf("> %s\n", secretName)
	}
	return nil
}

// edit a secret using $EDITOR.
func edit(nonInteractive bool, args []string) error {
	if len(args) != 1 {
		return errors.New("bad args: expected `edit $secret`")
	}
	if os.Getenv("EDITOR") == "" {
		return errors.New("must set $EDITOR")
	}
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}
	user, err := getUser(configPath)
	if err != nil {
		return errors.Wrap(err, "get user")
	}
	if nonInteractive {
		user.Password, err = requestPasswordFromServer(user.Port, false)
		if err != nil {
			return err
		}
	} else {
		user.Password, err = requestPassword(user.Port, defaultPasswordPrompt)
		if err != nil {
			return err
		}
	}
	keys, err := getKeys(configPath, user.Password)
	if err != nil {
		return err
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	secrets, err := shh.GetSecretsForUser(args[0], user.Username)
	if err != nil {
		return err
	}
	if len(secrets) > 1 {
		return errors.New("mulitple secrets found, cannot use *")
	}

	// Create tmp file
	fi, err := ioutil.TempFile("", "shh")
	if err != nil {
		return errors.Wrap(err, "temp file")
	}
	defer fi.Close()

	// Copy decrypted secret into tmp file
	var plaintext, aesKey []byte
	var key string
	for k, sec := range secrets {
		key = k

		// Decrypt the AES key using the private key
		aesKey, err = rsa.DecryptOAEP(sha256.New(), rand.Reader,
			keys.PrivateKey, []byte(sec.AESKey), nil)
		if err != nil {
			return errors.Wrap(err, "decrypt secret")
		}

		// Use the decrypted AES key to decrypt the secret
		aesBlock, err := aes.NewCipher(aesKey)
		if err != nil {
			return err
		}
		if len(sec.Encrypted) < aes.BlockSize {
			return errors.New("encrypted secret too short")
		}
		ciphertext := []byte(sec.Encrypted)
		iv := ciphertext[:aes.BlockSize]
		ciphertext = ciphertext[aes.BlockSize:]
		stream := cipher.NewCFBDecrypter(aesBlock, iv)
		plaintext = make([]byte, len(ciphertext))
		stream.XORKeyStream(plaintext, []byte(ciphertext))
	}
	if _, err = io.Copy(fi, bytes.NewReader(plaintext)); err != nil {
		return errors.Wrap(err, "copy")
	}

	// Checksum the plaintext, so we can exit early if nothing changed
	// (i.e. don't re-encrypt on saves without changes)
	h := sha1.New()
	if _, err = h.Write(plaintext); err != nil {
		return errors.Wrap(err, "write hash")
	}
	origHash := hex.EncodeToString(h.Sum(nil))

	// Open tmp file in vim
	cmd := exec.Command("bash", "-c", "$EDITOR "+fi.Name())
	cmd.Stdout = os.Stdout
	cmd.Stdin = os.Stdin
	if err = cmd.Start(); err != nil {
		return errors.Wrap(err, "cmd")
	}
	if err = cmd.Wait(); err != nil {
		return errors.Wrap(err, "wait")
	}

	// Check if the contents have changed. If not, we can exit early
	plaintext, err = ioutil.ReadFile(fi.Name())
	if err != nil {
		return errors.Wrap(err, "read all")
	}
	h = sha1.New()
	if _, err = h.Write(plaintext); err != nil {
		return errors.Wrap(err, "write hash")
	}
	newHash := hex.EncodeToString(h.Sum(nil))
	if origHash == newHash {
		return nil
	}

	// Re-encrypt content for each user with access to the secret
	for username, secrets := range shh.Secrets {
		if _, ok := secrets[key]; !ok {
			continue
		}

		// Generate an AES key to encrypt the data. We use AES-256
		// which requires a 32-byte key
		aesKey = make([]byte, 32)
		if _, err := rand.Read(aesKey); err != nil {
			return err
		}
		aesBlock, err := aes.NewCipher(aesKey)
		if err != nil {
			return err
		}

		// Encrypt the secret using the new AES key
		encrypted := make([]byte, aes.BlockSize+len(plaintext))
		iv := encrypted[:aes.BlockSize]
		if _, err := io.ReadFull(rand.Reader, iv); err != nil {
			return errors.Wrap(err, "read iv")
		}
		stream := cipher.NewCFBEncrypter(aesBlock, iv)
		stream.XORKeyStream(encrypted[aes.BlockSize:], []byte(plaintext))

		// Encrypt the AES key using the public key
		pubKey, err := x509.ParsePKCS1PublicKey(shh.Keys[username].Bytes)
		if err != nil {
			return errors.Wrap(err, "parse public key")
		}
		encryptedAES, err := rsa.EncryptOAEP(sha256.New(), rand.Reader,
			pubKey, aesKey, nil)
		if err != nil {
			return errors.Wrap(err, "reencrypt secret")
		}

		// We base64 encode all encrypted data before passing it into
		// the .shh file
		sec := secret{
			AESKey:    base64.StdEncoding.EncodeToString(encryptedAES),
			Encrypted: base64.StdEncoding.EncodeToString(encrypted),
		}
		shh.Secrets[username][key] = sec
	}
	return shh.EncodeToFile()
}

// rotate generates new keys and re-encrypts all secrets using the new keys.
// You should also use this to change your password.
func rotate(args []string) error {
	if len(args) != 0 {
		return errors.New("bad args: expected none")
	}

	// Allow changing the password
	oldPass, err := requestPassword(-1, "old password")
	if err != nil {
		return errors.Wrap(err, "request old password")
	}
	newPass, err := requestPasswordAndConfirm("new password")
	if err != nil {
		return errors.Wrap(err, "request new password")
	}

	configPath, err := getConfigPath()
	if err != nil {
		return err
	}

	// Generate new keys (different names). Note we do not use os.TempDir
	// because we'll be renaming the files later, and we can't rename files
	// across partitions (common for Linux)
	tmpDir := filepath.Join(configPath, "tmp")
	if err = os.Mkdir(tmpDir, 0777); err != nil {
		return errors.Wrap(err, "make tmp dir")
	}
	defer func() {
		os.RemoveAll(tmpDir)
	}()
	keys, err := createKeys(tmpDir, newPass)
	if err != nil {
		return errors.Wrap(err, "create keys")
	}
	user, err := getUser(configPath)
	if err != nil {
		return errors.Wrap(err, "get user")
	}

	// Decrypt all AES secrets for user, re-encrypt with new key
	oldKeys, err := getKeys(configPath, oldPass)
	if err != nil {
		return err
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	secrets := shh.Secrets[user.Username]
	for key, sec := range secrets {
		// Decrypt AES key using old key
		byt, err := base64.StdEncoding.DecodeString(sec.AESKey)
		if err != nil {
			return errors.Wrap(err, "decode base64")
		}
		aesKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader,
			oldKeys.PrivateKey, byt, nil)
		if err != nil {
			return errors.Wrap(err, "decrypt secret")
		}

		// Re-encrypt using new public key
		encryptedAES, err := rsa.EncryptOAEP(sha256.New(), rand.Reader,
			keys.PublicKey, aesKey, nil)
		if err != nil {
			return errors.Wrap(err, "reencrypt secret")
		}
		shh.Secrets[user.Username][key] = secret{
			AESKey:    base64.StdEncoding.EncodeToString(encryptedAES),
			Encrypted: sec.Encrypted,
		}
	}

	// Update public key in project file
	shh.Keys[user.Username] = keys.PublicKeyBlock

	// First create backups of our existing keys
	err = copyFile(
		filepath.Join(configPath, "id_rsa.bak"),
		filepath.Join(configPath, "id_rsa"),
	)
	if err != nil {
		return errors.Wrap(err, "back up id_rsa")
	}
	err = copyFile(
		filepath.Join(configPath, "id_rsa.pub.bak"),
		filepath.Join(configPath, "id_rsa.pub"),
	)
	if err != nil {
		return errors.Wrap(err, "back up id_rsa.pub")
	}

	// Rewrite the project file to use the new public key
	if err = shh.EncodeToFile(); err != nil {
		return errors.Wrap(err, "encode .shh")
	}

	// Move new keys on top of current keys in the filesystem
	err = os.Rename(
		filepath.Join(tmpDir, "id_rsa"),
		filepath.Join(configPath, "id_rsa"),
	)
	if err != nil {
		return errors.Wrap(err, "replace id_rsa")
	}
	err = os.Rename(
		filepath.Join(tmpDir, "id_rsa.pub"),
		filepath.Join(configPath, "id_rsa.pub"),
	)
	if err != nil {
		return errors.Wrap(err, "replace id_rsa.pub")
	}

	// Delete our backed up keys
	err = os.Remove(filepath.Join(configPath, "id_rsa.bak"))
	if err != nil {
		return errors.Wrap(err, "delete id_rsa.bak")
	}
	err = os.Remove(filepath.Join(configPath, "id_rsa.pub.bak"))
	if err != nil {
		return errors.Wrap(err, "delete id_rsa.pub.bak")
	}
	backupReminder(false)
	return nil
}

// addUser to project file.
func addUser(args []string) error {
	if len(args) != 0 && len(args) != 2 {
		return errors.New("bad args: expected `add-user [$user $pubkey]`")
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	var u *user
	if len(args) == 0 {
		// Default to self
		configPath, err := getConfigPath()
		if err != nil {
			return err
		}
		u, err = getUser(configPath)
		if err != nil {
			return errors.Wrap(err, "get user")
		}
	} else {
		u = &user{Username: username(args[0])}
	}
	if _, exist := shh.Keys[u.Username]; exist {
		return nil
	}
	if len(args) == 0 {
		shh.Keys[u.Username] = u.Keys.PublicKeyBlock
	} else {
		shh.Keys[u.Username], _ = pem.Decode([]byte(args[1]))
		if shh.Keys[u.Username] == nil {
			return errors.New("bad public key")
		}
	}
	return shh.EncodeToFile()
}

// rmUser from project file.
func rmUser(args []string) error {
	if len(args) != 1 {
		return errors.New("bad args: expected `rm-user $user`")
	}
	shh, err := shhFromPath(".shh")
	if err != nil {
		return err
	}
	username := username(args[0])
	if _, exist := shh.Keys[username]; !exist {
		return errors.New("user not found")
	}
	delete(shh.Keys, username)
	delete(shh.Secrets, username)
	return shh.EncodeToFile()
}

// serve maintains the password in memory for an hour.
func serve(args []string) error {
	if len(args) != 0 {
		return errors.New("bad args: expected none")
	}
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}
	user, err := getUser(configPath)
	if err != nil {
		return errors.Wrap(err, "get user")
	}
	const tickTime = time.Hour
	var mu sync.Mutex
	password := ""
	resetTicker := make(chan struct{})
	ticker := time.NewTicker(tickTime)
	go func() {
		for {
			select {
			case <-resetTicker:
				ticker.Stop()
				ticker = time.NewTicker(tickTime)
			case <-ticker.C:
				mu.Lock()
				password = ""
				mu.Unlock()
			}
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ping" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/reset-timer" {
			resetTicker <- struct{}{}
		}
		if r.Method == "GET" {
			_, _ = w.Write([]byte(password))
			return
		}
		byt, err := ioutil.ReadAll(r.Body)
		if len(byt) == 0 && err == nil {
			err = errors.New("empty body")
		}
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(err.Error()))
			return
		}
		password = string(byt)
		w.WriteHeader(http.StatusOK)
	})
	return http.ListenAndServe(fmt.Sprint(":", user.Port), mux)
}

// login to the server, caching the password in memory for 1 hour.
func login(args []string) error {
	if len(args) != 0 {
		return errors.New("bad args: expected none")
	}
	configPath, err := getConfigPath()
	if err != nil {
		return err
	}
	user, err := getUser(configPath)
	if err != nil {
		return errors.Wrap(err, "get user")
	}

	// Ensure the server is available
	url := fmt.Sprint("http://127.0.0.1:", user.Port)
	if err = pingServer(url); err != nil {
		return err
	}

	// Attempt to use cached password before asking again
	user.Password, err = requestPasswordFromServer(user.Port, true)
	if err == nil {
		return nil
	}

	user.Password, err = requestPassword(-1, defaultPasswordPrompt)
	if err != nil {
		return errors.Wrap(err, "request password")
	}

	// Verify the password before continuing
	if _, err = getKeys(configPath, user.Password); err != nil {
		return err
	}
	buf := bytes.NewBuffer(user.Password)
	resp, err := http.Post(url, "plaintext", buf)
	if err != nil {
		return errors.Wrap(err, "new request")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		body, _ := ioutil.ReadAll(resp.Body)
		return fmt.Errorf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	return nil
}

func copyFile(dst, src string) error {
	srcFi, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFi.Close()

	// Create the destination file with the same permissions as the source
	// file
	srcStat, err := srcFi.Stat()
	if err != nil {
		return err
	}
	dstFi, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE, srcStat.Mode())
	if err != nil {
		return err
	}
	defer dstFi.Close()

	_, err = io.Copy(dstFi, srcFi)
	return errors.Wrap(err, "copy")
}

func usage() {
	fmt.Println(`usage:

	shh [flags] [command]

global commands:
	init			initialize store or add self to existing store
	get $name		get secret
	set $name $val		set secret
	del $name		delete a secret
	allow $user $secret	allow user access to a secret
	deny $user $secret	deny user access to a secret
	add-user $user $pubkey  add user to project given their public key
	rm-user $user		remove user from project
	show [$user]		show user's allowed and denied keys
	edit			edit a secret using $EDITOR
	rotate			rotate key
	serve			start server to maintain password in memory
	login			login to server to maintain password in memory
	version			version information
	help			usage info

flags:
	-n			Non-interactive mode. Fail if shh would prompt for the password`)
}

func backupReminder(withConfig bool) {
	if withConfig {
		fmt.Println("> generated ~/.config/shh/config")
	}
	fmt.Println("> generated ~/.config/shh/id_rsa")
	fmt.Println("> generated ~/.config/shh/id_rsa.pub")
	fmt.Println(">")
	fmt.Println("> be sure to back up your ~/.config/shh/id_rsa and")
	fmt.Println("> remember your password, or you may lose access to your")
	fmt.Println("> secrets!")
}
