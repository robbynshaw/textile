package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ipfs/go-cid"

	pbar "github.com/cheggaaa/pb/v3"
	"github.com/ipfs/go-merkledag/dagutils"
	"github.com/ipfs/interface-go-ipfs-core/options"
	"github.com/ipfs/interface-go-ipfs-core/path"
	"github.com/logrusorgru/aurora"
	"github.com/manifoldco/promptui"
	"github.com/spf13/cobra"
	"github.com/textileio/go-threads/core/thread"
	"github.com/textileio/textile/api/buckets/client"
	pb "github.com/textileio/textile/api/buckets/pb"
	"github.com/textileio/textile/api/common"
	bucks "github.com/textileio/textile/buckets"
	"github.com/textileio/textile/buckets/local"
	"github.com/textileio/textile/cmd"
)

const nonFastForwardMsg = "the root of your bucket is behind (try `%s` before pushing again)"

var errNotABucket = fmt.Errorf("not a bucket (or any of the parent directories): .textile")

func init() {
	rootCmd.AddCommand(bucketCmd)
	bucketCmd.AddCommand(bucketInitCmd, bucketLinksCmd, bucketStatusCmd, bucketLsCmd, bucketPushCmd, bucketPullCmd, bucketCatCmd, bucketDestroyCmd)

	bucketInitCmd.PersistentFlags().String("key", "", "Bucket key")
	bucketInitCmd.PersistentFlags().String("org", "", "Org username")
	bucketInitCmd.PersistentFlags().Bool("public", false, "Allow public access")
	bucketInitCmd.PersistentFlags().String("thread", "", "Thread ID")
	bucketInitCmd.Flags().Bool("existing", false, "If set, initializes from an existing remote bucket")

	if err := cmd.BindFlags(configViper, bucketInitCmd, flags); err != nil {
		cmd.Fatal(err)
	}
}

var bucketCmd = &cobra.Command{
	Use:   "bucket",
	Short: "Manage a storage bucket",
	Long:  `Manage files and folders with an object storage bucket.`,
	Args:  cobra.ExactArgs(0),
}

var bucketInitCmd = &cobra.Command{
	Use:   "init",
	Short: "Create an empty bucket",
	Long: `Create an empty storage bucket.

A .textile config directory and a seed file will be created in the current working directory.
Existing configs will not be overwritten.

Use the '--existing' flag to initialize from an existing remote bucket.
`,
	Args: cobra.ExactArgs(0),
	PreRun: func(c *cobra.Command, args []string) {
		cmd.ExpandConfigVars(configViper, flags)
	},
	Run: func(c *cobra.Command, args []string) {
		root, err := os.Getwd()
		if err != nil {
			cmd.Fatal(err)
		}
		dir := filepath.Join(root, configDir)
		if err = os.MkdirAll(dir, os.ModePerm); err != nil {
			cmd.Fatal(err)
		}
		filename := filepath.Join(dir, "config.yml")
		if _, err := os.Stat(filename); err == nil {
			cmd.Fatal(fmt.Errorf("bucket %s is already initialized", root))
		}

		existing, err := c.Flags().GetBool("existing")
		if err != nil {
			cmd.Fatal(err)
		}
		if existing {
			ctx, cancel := authCtx(cmdTimeout)
			defer cancel()
			res, err := users.ListThreads(ctx)
			if err != nil {
				cmd.Fatal(err)
			}

			type bucketInfo struct {
				ID   thread.ID
				Name string
				Key  string
			}
			bucketInfos := make([]bucketInfo, 0)
			for _, reply := range res.List {
				id, err := thread.Cast(reply.ID)
				if err != nil {
					cmd.Fatal(err)
				}
				ctx = common.NewThreadIDContext(ctx, id)
				res, err := buckets.List(ctx)
				if err != nil {
					cmd.Fatal(err)
				}
				for _, root := range res.Roots {
					name := "unnamed"
					if root.Name != "" {
						name = root.Name
					}
					bucketInfos = append(bucketInfos, bucketInfo{ID: id, Name: name, Key: root.Key})
				}
			}

			prompt := promptui.Select{
				Label: "Which exiting bucket do you want to init from?",
				Items: bucketInfos,
				Templates: &promptui.SelectTemplates{
					Active:   fmt.Sprintf(`{{ "%s" | cyan }} {{ .Name | bold }} {{ .Key | faint | bold }}`, promptui.IconSelect),
					Inactive: `{{ .Name | faint }} {{ .Key | faint | bold }}`,
					Selected: aurora.Sprintf(aurora.BrightBlack("> Selected bucket {{ .Name | white | bold }}")),
				},
			}
			index, _, err := prompt.Run()
			if err != nil {
				cmd.Fatal(err)
			}

			selected := bucketInfos[index]
			configViper.Set("thread", selected.ID.String())
			configViper.Set("key", selected.Key)
		}

		var dbID thread.ID
		xthread := configViper.GetString("thread")
		if configViper.GetString("thread") != "" {
			var err error
			dbID, err = thread.Decode(xthread)
			if err != nil {
				cmd.Fatal(fmt.Errorf("invalid thread ID"))
			}
		}

		xkey := configViper.GetString("key")
		initRemote := true
		if xkey != "" {
			if !dbID.Defined() {
				cmd.Fatal(fmt.Errorf("the --thread flag is required when using --key"))
			}
			initRemote = false
		}

		var name string
		if initRemote {
			prompt := promptui.Prompt{
				Label: "Enter a name for your new bucket (optional)",
			}
			var err error
			name, err = prompt.Run()
			if err != nil {
				cmd.End("")
			}
		}

		if !dbID.Defined() {
			selected := selectThread("Buckets are written to a threadDB. Select or create a new one", aurora.Sprintf(
				aurora.BrightBlack("> Selected threadDB {{ .ID | white | bold }}")), true)
			if selected.ID == "Create new" {
				if selected.Name == "" {
					prompt := promptui.Prompt{
						Label: "Enter a name for your new threadDB (optional)",
					}
					var err error
					selected.Name, err = prompt.Run()
					if err != nil {
						cmd.End("")
					}
				}
				ctx, cancel := threadCtx(cmdTimeout)
				defer cancel()
				dbID = thread.NewIDV1(thread.Raw, 32)
				ctx = common.NewThreadNameContext(ctx, selected.Name)
				if err := threads.NewDB(ctx, dbID); err != nil {
					cmd.Fatal(err)
				}
			} else {
				var err error
				dbID, err = thread.Decode(selected.ID)
				if err != nil {
					cmd.Fatal(err)
				}
			}
			configViper.Set("thread", dbID.String())
		}

		if initRemote {
			ctx, cancel := threadCtx(cmdTimeout)
			defer cancel()
			rep, err := buckets.Init(ctx, name)
			if err != nil {
				cmd.Fatal(err)
			}
			configViper.Set("key", rep.Root.Key)

			file, err := os.Create(filepath.Join(root, bucks.SeedName))
			if err != nil {
				cmd.Fatal(err)
			}
			defer file.Close()
			_, err = file.Write(rep.Seed)
			if err != nil {
				cmd.Fatal(err)
			}
			if _, err = file.Seek(0, 0); err != nil {
				cmd.Fatal(err)
			}
			buck, err := local.NewBucket(root, options.BalancedLayout)
			if err != nil {
				cmd.Fatal(err)
			}
			actx, acancel := context.WithTimeout(context.Background(), cmdTimeout)
			defer acancel()
			if err = buck.ArchiveFile(actx, file, bucks.SeedName); err != nil {
				cmd.Fatal(err)
			}

			printLinks(rep.Links)
		}

		if err := configViper.WriteConfigAs(filename); err != nil {
			cmd.Fatal(err)
		}
		if existing {
			key := configViper.GetString("key")
			count := getPath(key, ".", ".", root, nil)
			cmd.Success("Initialized from remote and pulled %d files to %s", aurora.White(count).Bold(), aurora.White(root).Bold())
		} else {
			cmd.Success("Initialized an empty bucket in %s", aurora.White(root).Bold())
		}
	},
}

func printLinks(reply *pb.LinksReply) {
	cmd.Message("Your bucket links:")
	cmd.Message("%s Thread link", aurora.White(reply.URL).Bold())
	if reply.WWW != "" {
		cmd.Message("%s Bucket website", aurora.White(reply.WWW).Bold())
	}
	cmd.Message("%s IPNS website (propagation can be slow)", aurora.White(reply.IPNS).Bold())
}

var bucketLinksCmd = &cobra.Command{
	Use:   "links",
	Short: "Print links to where this bucket can be accessed",
	Long:  `Print links to where this storage bucket can be accessed.`,
	Args:  cobra.ExactArgs(0),
	PreRun: func(c *cobra.Command, args []string) {
		cmd.ExpandConfigVars(configViper, flags)
		if configViper.ConfigFileUsed() == "" {
			cmd.Fatal(errNotABucket)
		}
	},
	Run: func(c *cobra.Command, args []string) {
		ctx, cancel := threadCtx(cmdTimeout)
		defer cancel()
		key := configViper.GetString("key")
		reply, err := buckets.Links(ctx, key)
		if err != nil {
			cmd.Fatal(err)
		}
		printLinks(reply)
	},
}

var bucketStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show bucket object changes",
	Long:  `Displays paths that have been added to and paths that have been removed or differ from the current bucket root.`,
	Args:  cobra.ExactArgs(0),
	PreRun: func(c *cobra.Command, args []string) {
		cmd.ExpandConfigVars(configViper, flags)
	},
	Run: func(c *cobra.Command, args []string) {
		conf := configViper.ConfigFileUsed()
		if conf == "" {
			cmd.Fatal(errNotABucket)
		}
		root := filepath.Dir(filepath.Dir(conf))

		buck, err := local.NewBucket(root, options.BalancedLayout)
		if err != nil {
			cmd.Fatal(err)
		}
		cmd.Message("Current root: %s", buck.Path())
		diff := getDiff(buck, root)
		for _, c := range diff {
			cf := changeColor(c.Type)
			cmd.Message("%s  %s", cf(changeType(c.Type)), cf(c.Path))
		}
	},
}

func getDiff(buck *local.Bucket, root string) []*dagutils.Change {
	ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
	defer cancel()
	diff, err := buck.Diff(ctx, root)
	if err != nil {
		cmd.Fatal(err)
	}
	if len(diff) == 0 {
		cmd.End("Everything up-to-date")
	}
	var all []*dagutils.Change
	for _, c := range diff {
		switch c.Type {
		case dagutils.Mod, dagutils.Add:
			names := walkPath(c.Path)
			if len(names) > 0 {
				for _, n := range names {
					all = append(all, &dagutils.Change{Type: c.Type, Path: n})
				}
			} else {
				all = append(all, c)
			}
		case dagutils.Remove:
			all = append(all, c)
		}
	}
	return all
}

func changeType(t dagutils.ChangeType) string {
	switch t {
	case dagutils.Mod:
		return "modified:"
	case dagutils.Add:
		return "new file:"
	case dagutils.Remove:
		return "deleted: "
	default:
		return ""
	}
}

func changeColor(t dagutils.ChangeType) func(arg interface{}) aurora.Value {
	switch t {
	case dagutils.Mod:
		return aurora.Yellow
	case dagutils.Add:
		return aurora.Green
	case dagutils.Remove:
		return aurora.Red
	default:
		return nil
	}
}

var bucketLsCmd = &cobra.Command{
	Use: "ls [path]",
	Aliases: []string{
		"list",
	},
	Short: "List bucket objects under a path",
	Long:  `List bucket objects under a path.`,
	Args:  cobra.MaximumNArgs(1),
	PreRun: func(c *cobra.Command, args []string) {
		cmd.ExpandConfigVars(configViper, flags)
		if configViper.ConfigFileUsed() == "" {
			cmd.Fatal(errNotABucket)
		}
	},
	Run: func(c *cobra.Command, args []string) {
		ctx, cancel := threadCtx(cmdTimeout)
		defer cancel()

		var pth string
		if len(args) > 0 {
			pth = args[0]
		}
		if pth == "." || pth == "/" || pth == "./" {
			pth = ""
		}
		key := configViper.GetString("key")
		rep, err := buckets.ListPath(ctx, key, pth)
		if err != nil {
			cmd.Fatal(err)
		}
		var items []*pb.ListPathReply_Item
		if len(rep.Item.Items) > 0 {
			items = rep.Item.Items
		} else if !rep.Item.IsDir {
			items = append(items, rep.Item)
		}

		var data [][]string
		if len(items) > 0 && !strings.HasPrefix(pth, configDir) {
			for _, item := range items {
				if item.Name == configDir {
					continue
				}
				var links string
				if item.IsDir {
					links = strconv.Itoa(len(item.Items))
				} else {
					links = "n/a"
				}
				data = append(data, []string{
					item.Name,
					strconv.Itoa(int(item.Size)),
					strconv.FormatBool(item.IsDir),
					links,
					item.Path,
				})
			}
		}

		if len(data) > 0 {
			cmd.RenderTable([]string{"name", "size", "dir", "objects", "path"}, data)
		}
		cmd.Message("Found %d objects", aurora.White(len(data)).Bold())
	},
}

var bucketPushCmd = &cobra.Command{
	Use:   "push",
	Short: "Push bucket object changes",
	Long:  `Push bucket object changes. Directory structure is mirrored in the bucket.`,
	Args:  cobra.ExactArgs(0),
	PreRun: func(c *cobra.Command, args []string) {
		cmd.ExpandConfigVars(configViper, flags)
	},
	Run: func(c *cobra.Command, args []string) {
		conf := configViper.ConfigFileUsed()
		if conf == "" {
			cmd.Fatal(errNotABucket)
		}
		root := filepath.Dir(filepath.Dir(conf))

		dbID := getThreadID()
		if !dbID.Defined() {
			cmd.Fatal(fmt.Errorf("thread is not defined"))
		}

		buck, err := local.NewBucket(root, options.BalancedLayout)
		if err != nil {
			cmd.Fatal(err)
		}
		diff := getDiff(buck, root)

		cmd.Message("Changes to be pushed:")
		for _, c := range diff {
			cf := changeColor(c.Type)
			cmd.Message("%s  %s", cf(changeType(c.Type)), cf(c.Path))
		}
		prompt := promptui.Prompt{
			Label:     fmt.Sprintf("Push %d changes", len(diff)),
			IsConfirm: true,
		}
		if _, err := prompt.Run(); err != nil {
			cmd.End("")
		}

		key := configViper.GetString("key")
		xr := buck.Path()
		for _, c := range diff {
			switch c.Type {
			case dagutils.Mod, dagutils.Add:
				xr = addFile(key, xr, filepath.Join(root, c.Path), c.Path)
			case dagutils.Remove:
				xr = rmFile(key, xr, c.Path)
			}
		}

		ctx, cancel := context.WithTimeout(context.Background(), cmdTimeout)
		defer cancel()
		if err = buck.Archive(ctx); err != nil {
			cmd.Fatal(err)
		}

		cmd.Message("New root: %s", buck.Path())
		cmd.Success("Pushed %d changes", aurora.White(len(diff)).Bold())
	},
}

func walkPath(pth string) (names []string) {
	if err := filepath.Walk(pth, func(n string, info os.FileInfo, err error) error {
		if err != nil {
			cmd.Fatal(err)
		}
		if !info.IsDir() {
			if local.Ignore(n) || strings.HasPrefix(n, configDir) {
				return nil
			}
			names = append(names, n)
		}
		return nil
	}); err != nil {
		cmd.Fatal(err)
	}
	return names
}

func addFile(key string, xroot path.Resolved, name, filePath string) path.Resolved {
	file, err := os.Open(name)
	if err != nil {
		cmd.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		cmd.Fatal(err)
	}

	bar := pbar.New(int(info.Size()))
	bar.SetTemplate(pbar.Full)
	bar.Set(pbar.Bytes, true)
	bar.Set(pbar.SIBytesPrefix, true)
	bar.Start()
	progress := make(chan int64)
	root := make(chan path.Resolved)

	go func() {
		ctx, cancel := threadCtx(addFileTimeout)
		defer cancel()
		_, r, err := buckets.PushPath(ctx, key, filePath, file, client.WithFastForwardOnly(xroot), client.WithProgress(progress))
		if err != nil {
			if strings.HasSuffix(err.Error(), bucks.ErrNonFastForward.Error()) {
				cmd.Fatal(errors.New(nonFastForwardMsg), aurora.Cyan("tt bucket pull"))
			} else {
				cmd.Fatal(err)
			}
		}
		root <- r
	}()

	for up := range progress {
		bar.SetCurrent(up)
	}
	bar.Finish()

	return <-root
}

func rmFile(key string, xroot path.Resolved, filePath string) path.Resolved {
	ctx, cancel := threadCtx(addFileTimeout)
	defer cancel()
	root, err := buckets.RemovePath(ctx, key, filePath, client.WithFastForwardOnly(xroot))
	if err != nil {
		cmd.Fatal(err)
	}
	return root
}

var bucketPullCmd = &cobra.Command{
	Use:   "pull [path]",
	Short: "Pull bucket object changes under path",
	Long:  `Pull bucket object changes under path. Bucket structure is mirrored locally.`,
	Args:  cobra.MaximumNArgs(0),
	PreRun: func(c *cobra.Command, args []string) {
		cmd.ExpandConfigVars(configViper, flags)
	},
	Run: func(c *cobra.Command, args []string) {
		conf := configViper.ConfigFileUsed()
		if conf == "" {
			cmd.Fatal(errNotABucket)
		}
		root := filepath.Dir(filepath.Dir(conf))
		buck, err := local.NewBucket(root, options.BalancedLayout)
		if err != nil {
			cmd.Fatal(err)
		}
		var pth string
		if len(args) > 0 {
			pth = args[0]
		}
		if pth == "." || pth == "/" || pth == "./" {
			pth = ""
		}
		key := configViper.GetString("key")
		count := getPath(key, pth, filepath.Dir(pth), root, buck)
		cmd.Success("Pulled %d changes", aurora.White(count).Bold())
	},
}

func getPath(key, pth, dir, dest string, buck *local.Bucket) (count int) {
	ctx, cancel := threadCtx(cmdTimeout)
	defer cancel()
	rep, err := buckets.ListPath(ctx, key, pth)
	if err != nil {
		cmd.Fatal(err)
	}

	if rep.Item.IsDir {
		for _, i := range rep.Item.Items {
			count += getPath(key, filepath.Join(pth, filepath.Base(i.Path)), dir, dest, buck)
		}
	} else {
		if dir != "." {
			pth = strings.TrimPrefix(pth, dir)
		}
		name := filepath.Join(dest, pth)

		if buck != nil {
			c, err := cid.Decode(rep.Item.Cid)
			if err != nil {
				cmd.Fatal(err)
			}
			lc, err := buck.HashFile(name)
			if err == nil && lc.Equals(c) { // File exists
				return
			}
		}
		getFile(key, pth, name, rep.Item.Size)
		count++
	}
	return count
}

func getFile(key, filePath, name string, size int64) {
	if err := os.MkdirAll(filepath.Dir(name), os.ModePerm); err != nil {
		cmd.Fatal(err)
	}
	file, err := os.Create(name)
	if err != nil {
		cmd.Fatal(err)
	}
	defer file.Close()

	bar := pbar.New(int(size))
	bar.SetTemplate(pbar.Full)
	bar.Set(pbar.Bytes, true)
	bar.Set(pbar.SIBytesPrefix, true)
	bar.Start()
	progress := make(chan int64)

	go func() {
		ctx, cancel := threadCtx(getFileTimeout)
		defer cancel()
		if err = buckets.PullPath(ctx, key, filePath, file, client.WithProgress(progress)); err != nil {
			cmd.Fatal(err)
		}
	}()

	for up := range progress {
		bar.SetCurrent(up)
	}
	bar.Finish()
}

var bucketCatCmd = &cobra.Command{
	Use:   "cat [path]",
	Short: "Cat bucket object at path",
	Long:  `Cat bucket object at path.`,
	Args:  cobra.ExactArgs(1),
	PreRun: func(c *cobra.Command, args []string) {
		cmd.ExpandConfigVars(configViper, flags)
		if configViper.ConfigFileUsed() == "" {
			cmd.Fatal(errNotABucket)
		}
	},
	Run: func(c *cobra.Command, args []string) {
		ctx, cancel := threadCtx(getFileTimeout)
		defer cancel()
		key := configViper.GetString("key")
		if err := buckets.PullPath(ctx, key, args[0], os.Stdout); err != nil {
			cmd.Fatal(err)
		}
	},
}

var bucketDestroyCmd = &cobra.Command{
	Use:   "destroy",
	Short: "Destroy bucket",
	Long:  `Destroy bucket and all associated data.`,
	Args:  cobra.ExactArgs(0),
	PreRun: func(c *cobra.Command, args []string) {
		cmd.ExpandConfigVars(configViper, flags)
		if configViper.ConfigFileUsed() == "" {
			cmd.Fatal(errNotABucket)
		}
	},
	Run: func(c *cobra.Command, args []string) {
		cmd.Warn("%s", aurora.Red("This action cannot be undone. The bucket and all associated data will be permanently deleted."))
		prompt := promptui.Prompt{
			Label:     fmt.Sprintf("Are you absolutely sure"),
			IsConfirm: true,
		}
		if _, err := prompt.Run(); err != nil {
			cmd.End("")
		}

		ctx, cancel := threadCtx(cmdTimeout)
		defer cancel()
		key := configViper.GetString("key")
		if err := buckets.Remove(ctx, key); err != nil {
			cmd.Fatal(err)
		}
		_ = os.RemoveAll(configViper.ConfigFileUsed())
		cmd.Success("Your bucket has been deleted")
	},
}
